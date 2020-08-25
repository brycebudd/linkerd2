package proxyinjector

import (
	"context"

	"github.com/linkerd/linkerd2/controller/k8s"
	injector "github.com/linkerd/linkerd2/controller/proxy-injector"
	"github.com/linkerd/linkerd2/controller/webhook"
)

// Main executes the proxy-injector subcommand
func Main(args []string) {

	// TODO: load values
	injection := &injector.Injection{}

	webhook.Launch(
		context.Background(),
		[]k8s.APIResource{k8s.NS, k8s.Deploy, k8s.RC, k8s.RS, k8s.Job, k8s.DS, k8s.SS, k8s.Pod, k8s.CJ},
		9995,
		injection.Inject,
		"linkerd-proxy-injector",
		"proxy-injector",
		args,
	)
}
