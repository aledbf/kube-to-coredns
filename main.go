package main

import (
	"os"

	"github.com/golang/glog"
	flag "github.com/spf13/pflag"

	"k8s.io/kubernetes/pkg/client/unversioned"
	kubectl_util "k8s.io/kubernetes/pkg/kubectl/cmd/util"
)

var (
	flags = flag.NewFlagSet("", flag.ContinueOnError)

	cluster = flags.Bool("use-kubernetes-cluster-service", true, `If true, use the built in kubernetes
        cluster for creating the client`)

	argDomain = flag.String("domain", "cluster.local", "domain under which to create names")
)

func main() {
	clientConfig := kubectl_util.DefaultClientConfig(flags)
	flags.Parse(os.Args)

	var err error
	var kubeClient *unversioned.Client

	if *cluster {
		if kubeClient, err = unversioned.NewInCluster(); err != nil {
			glog.Fatalf("Failed to create client: %v", err)
		}
	} else {
		config, err := clientConfig.ClientConfig()
		if err != nil {
			glog.Fatalf("error connecting to the client: %v", err)
		}
		kubeClient, err = unversioned.New(config)
	}

	domain := *argDomain
	if !strings.HasSuffix(domain, ".") {
		domain = fmt.Sprintf("%s.", domain)
	}

	ks := newdnsController(domain, kubeClient, resyncPeriod)
	ks.Run()
}
