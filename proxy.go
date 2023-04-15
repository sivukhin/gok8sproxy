package gok8sproxy

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/intstr"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	v12 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type ForwardPoint struct {
	ip       net.IP
	shutdown chan<- struct{}
}

type ForwardPoints struct {
	lock   sync.Mutex
	config *rest.Config
	client *kubernetes.Clientset
	points map[string]ForwardPoint
	logger *zap.SugaredLogger
}

func NewForwardPoints(config *rest.Config, client *kubernetes.Clientset, logger *zap.SugaredLogger) *ForwardPoints {
	return &ForwardPoints{
		config: config,
		client: client,
		points: make(map[string]ForwardPoint),
		logger: logger,
	}
}

type DnsHandler struct {
	dnsAddress    string
	forwardPoints *ForwardPoints
	logger        *zap.SugaredLogger
}

func NewDnsHandler(dnsAddress string, forwardPoints *ForwardPoints, logger *zap.SugaredLogger) *DnsHandler {
	return &DnsHandler{
		dnsAddress:    dnsAddress,
		forwardPoints: forwardPoints,
		logger:        logger,
	}
}

type DnsProxy struct {
	address *net.UDPAddr
	server  *dns.Server
}

type DnsProxyPool struct {
	lock          sync.Mutex
	proxies       map[string]DnsProxy
	forwardPoints *ForwardPoints
	logger        *zap.SugaredLogger
}

func NewDnsProxyPool(forwardPoints *ForwardPoints, logger *zap.SugaredLogger) *DnsProxyPool {
	return &DnsProxyPool{
		proxies:       make(map[string]DnsProxy),
		forwardPoints: forwardPoints,
		logger:        logger,
	}
}

func parseK8sDomain(domain string) (namespace, service string, err error) {
	prefix, ok := strings.CutSuffix(domain, k8sDnsNameSuffix)
	if !ok {
		return "", "", fmt.Errorf("unable to parse domain")
	}
	tokens := strings.Split(prefix, ".")
	if len(tokens) != 2 {
		return "", "", fmt.Errorf("domain prefix must have exactly two tokens")
	}
	return tokens[1], tokens[0], nil
}

func createForwarderPorts(serviceObject *v12.Service, podObject v12.Pod) []string {
	tcpPorts := make([]string, 0)
	for _, port := range serviceObject.Spec.Ports {
		if port.Protocol == v12.ProtocolTCP {
			targetPort := int32(-1)
			if port.TargetPort.Type == intstr.Int {
				targetPort = port.TargetPort.IntVal
			} else {
				for _, container := range podObject.Spec.Containers {
					for _, containerPort := range container.Ports {
						if containerPort.Name == port.TargetPort.String() {
							targetPort = containerPort.ContainerPort
							break
						}
					}
				}
			}
			if targetPort >= 0 {
				tcpPorts = append(tcpPorts, fmt.Sprintf("%v:%v", port.Port, targetPort))
			}
		}
	}
	return tcpPorts
}

func (f *ForwardPoints) Shutdown() {
	f.logger.Infof("shutting down all forward points")
	for domain, point := range f.points {
		f.logger.Infof("shutting down forward point %v: ip=%v", domain, point.ip)
		point.shutdown <- struct{}{}
	}
}

type zapWriter struct {
	prefix string
	write  func(template string, args ...interface{})
}

func (w *zapWriter) Write(p []byte) (n int, err error) {
	w.write("%v: %v", w.prefix, string(p))
	return len(p), nil
}

func (f *ForwardPoints) Get(domain string) (net.IP, error) {
	{
		f.lock.Lock()
		point, ok := f.points[domain]
		f.lock.Unlock()

		if ok {
			return point.ip, nil
		}
	}
	transport, upgrader, err := spdy.RoundTripperFor(f.config)
	if err != nil {
		return nil, fmt.Errorf("%v: unable to create round-tripper: %w", domain, err)
	}
	namespace, service, err := parseK8sDomain(domain)
	if err != nil {
		return nil, fmt.Errorf("%v: unable to parse k8s domain: %w", domain, err)
	}
	serviceObject, err := f.client.CoreV1().Services(namespace).Get(context.Background(), service, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%v: unable to get service %v: %w", domain, service, err)
	}
	podSelector := labels.SelectorFromSet(serviceObject.Spec.Selector)
	pods, err := f.client.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{LabelSelector: podSelector.String()})
	if err != nil {
		return nil, fmt.Errorf("%v: unable to get service %v pods by selector %v", domain, service, podSelector)
	}
	f.logger.Infof("%v: found %v service pods", domain, len(pods.Items))
	var targetPod v12.Pod
	for _, pod := range pods.Items {
		if pod.Status.Phase == v12.PodRunning {
			targetPod = pod
			break
		}
	}
	if targetPod.Status.Phase != v12.PodRunning {
		return nil, fmt.Errorf("%v: no running pods for service %v", domain, service)
	}
	f.logger.Infof("%v: found target pod %v", domain, targetPod.Name)
	url := f.client.
		RESTClient().
		Post().
		Prefix("api", "v1").
		Resource("pods").
		Namespace(namespace).
		Name(targetPod.Name).
		SubResource("portforward").
		URL()

	ip := net.IPv4(127, byte(1+rand.Intn(255)), byte(1+rand.Intn(255)), 0)
	f.logger.Infof("%v: chose proxy address: ip=%v", domain, ip)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	shutdown, ready := make(chan struct{}), make(chan struct{})

	ports := createForwarderPorts(serviceObject, targetPod)
	f.logger.Infof("%v: found service %v: ports=%v, selector=%v", domain, serviceObject.Name, ports, podSelector)
	fw, err := portforward.NewOnAddresses(
		dialer,
		[]string{ip.String()},
		ports,
		shutdown,
		ready,
		&zapWriter{"forward debug", f.logger.Debugf},
		&zapWriter{"forward error", f.logger.Errorf},
	)
	go func() {
		err := fw.ForwardPorts()
		if err != nil {
			f.logger.Errorf("%v: unable to forward ports: %v", domain, err)
		}
	}()
	<-ready
	f.logger.Infof("%v: port-forwarder ready", domain)
	{
		f.lock.Lock()
		f.points[domain] = ForwardPoint{ip: ip, shutdown: shutdown}
		f.lock.Unlock()
	}
	return ip, nil
}

const (
	k8sDnsNameSuffix = ".svc.cluster.local."
)

func extractK8sDomains(message *dns.Msg) (*dns.Msg, []dns.Question) {
	regular, k8s := make([]dns.Question, 0), make([]dns.Question, 0)
	for _, q := range message.Question {
		if strings.HasSuffix(q.Name, k8sDnsNameSuffix) && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA) {
			k8s = append(k8s, q)
		} else {
			regular = append(regular, q)
		}
	}
	filtered := message.Copy()
	filtered.Question = regular
	return filtered, k8s
}

func sendNonEmptyRequest(address string, r *dns.Msg) (*dns.Msg, error) {
	if len(r.Question) == 0 {
		r.Response = true
		return r, nil
	}
	client := new(dns.Client)
	connection, err := client.Dial(address)
	if err != nil {
		return nil, fmt.Errorf("%v: unable to dial connection: %v", address, err)
	}
	defer connection.Close()

	pack, err := r.Pack()
	if err != nil {
		return nil, fmt.Errorf("%v: unable to pack DNS message: %v", address, err)
	}
	n, err := connection.Write(pack)
	if err != nil {
		return nil, fmt.Errorf("%v: unable to send DNS message: %v", address, err)
	}
	if n != len(pack) {
		return nil, fmt.Errorf("%v: unable to send DNS message as single UDP datagram", address)
	}
	response, err := connection.ReadMsg()
	if err != nil {
		return nil, fmt.Errorf("%v: unable to read DNS response: %v", address, err)
	}
	return response, nil
}

func (h *DnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	defer w.Close()

	filteredRequest, k8s := extractK8sDomains(r)
	response, err := sendNonEmptyRequest(h.dnsAddress, filteredRequest)
	if err != nil {
		h.logger.Errorf("unable to proxy request: %v", err)
		return
	}
	response.Question = r.Question

	for _, q := range k8s {
		ip, err := h.forwardPoints.Get(q.Name)
		if err != nil {
			h.logger.Errorf("%v: unable to create forward point: %v", q.Name, err)
		} else {
			response.Answer = append(response.Answer, &dns.A{
				Hdr: dns.RR_Header{
					Name:     q.Name,
					Rrtype:   q.Qtype,
					Class:    q.Qclass,
					Ttl:      60,
					Rdlength: 0,
				},
				A: ip,
			})
		}
	}
	err = w.WriteMsg(response)
	if err != nil {
		h.logger.Errorf("unable to process dns request: %v", err)
	}
}

func randomPort() int { return 1000 + rand.Intn(65536-1000) }

func (p *DnsProxyPool) Get(dnsAddress string) (DnsProxy, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if proxy, ok := p.proxies[dnsAddress]; ok {
		return proxy, nil
	}
	p.logger.Infof("%v: use dns server for the first time", dnsAddress)

	handler := NewDnsHandler(dnsAddress, p.forwardPoints, p.logger)
	proxyAddress := fmt.Sprintf("127.0.0.1:%v", randomPort())
	proxyAddrPort, err := netip.ParseAddrPort(proxyAddress)
	if err != nil {
		return DnsProxy{}, err
	}
	proxyUdpAddr := net.UDPAddrFromAddrPort(proxyAddrPort)
	ready := make(chan struct{})
	dnsServer := &dns.Server{Addr: proxyAddress, Net: "udp", Handler: handler, NotifyStartedFunc: func() { ready <- struct{}{} }}
	go func() {
		err := dnsServer.ListenAndServe()
		if err != nil {
			fmt.Printf("err: %v\n", err)
		}
	}()
	<-ready
	p.logger.Infof("%v: proxy for dns server is ready at address %v", dnsAddress, proxyAddress)
	proxy := DnsProxy{address: proxyUdpAddr, server: dnsServer}
	p.proxies[dnsAddress] = proxy
	return proxy, nil
}

func (p *DnsProxyPool) Shutdown() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.logger.Infof("shutting down all dns proxies")
	for domain, proxy := range p.proxies {
		p.logger.Infof("shutting down dns proxy %v: address=%v", domain, proxy.address)
		err := proxy.server.Shutdown()
		if err != nil {
			fmt.Printf("err: %v\n", err)
		}
	}
}

func SetupDnsProxy(logger *zap.SugaredLogger) (cancel func(), err error) {
	kubeConfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to build k8s config for dns proxy: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create k8s client for dns proxy: %w", err)
	}
	forwardPoints := NewForwardPoints(config, client, logger)
	dnsPool := NewDnsProxyPool(forwardPoints, logger)
	net.DefaultResolver = &net.Resolver{
		PreferGo:     true,
		StrictErrors: false,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			proxy, err := dnsPool.Get(address)
			if err != nil {
				return nil, err
			}
			return net.DialUDP(network, nil, proxy.address)
		},
	}
	logger.Info("net.DefaultResolver was replaced by k8s interceptor")
	return func() {
		forwardPoints.Shutdown()
		dnsPool.Shutdown()
	}, nil
}

func MustSetupDnsProxy(logger *zap.SugaredLogger) func() {
	cancel, err := SetupDnsProxy(logger)
	if err != nil {
		panic(fmt.Errorf("unable to setup k8s dns proxy: %w", err))
	}
	return cancel
}
