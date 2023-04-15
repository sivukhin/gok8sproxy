# gok8sproxy

**gok8sproxy** is a Kubernetes DNS Proxy library that allows to resolve in-cluster service addresses to localhost IP
addresses with port forwarding. It can be used to access Kubernetes services from your local development environment
without the need for additional proxy tools.

## Prerequisites

- Go 1.13 or higher
- Kubernetes client configuration file (`kubeconfig`)

## Installation

To use the **gok8sproxy** library, simply import it in your Go project:

```go
import "github.com/sivukhin/gok8sproxy"
```

## Usage

For example, if you want to write simple test that access k8s service in remote cluster you can use **gok8sproxy** like
the following:

```go
func TestRemoteAccess(t *testing.T) {
    cancel := gok8sproxy.MustSetupDnsProxy(zaptest.NewLogger(t).Sugar())
    defer cancel()
    
    response, err := http.Get("http://your-service.your-namespace.svc.cluster.local")
    t.Logf("r: %v, err: %v", response, err)
}
```

## How gok8sproxy works

**gok8sproxy** works by intercepting DNS requests (replacing go `net.DefaultResolver`) and forwarding them to the
appropriate Kubernetes services or external DNS servers.