# gok8sproxy

**gok8sproxy** is a Kubernetes DNS Proxy library that allows to resolve in-cluster service addresses to localhost IP
addresses with port forwarding. It can be used to access Kubernetes services from your local development environment
without the need for additional proxy tools.

## Prerequisites

- Go 1.18 or higher
- Kubernetes client configuration file (`kubeconfig`)

## Installation

Install **gok8sproxy** in your project with command:
```bash
go get github.com/sivukhin/gok8sproxy
```

Alternatively, you can import it and let your IDE do all the work:

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
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    body, err := io.ReadAll(response.Body)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    t.Logf("k8s service response: %v", string(body))
}
```

## How gok8sproxy works

**gok8sproxy** works by intercepting DNS requests (replacing go `net.DefaultResolver`) and forwarding them to the appropriate Kubernetes services or external DNS servers.