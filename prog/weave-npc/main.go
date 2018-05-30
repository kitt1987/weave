package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	coreapi "k8s.io/api/core/v1"
	extnapi "k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/weaveworks/weave/common"
	"github.com/weaveworks/weave/net/ipset"
	"github.com/weaveworks/weave/npc"
	"github.com/weaveworks/weave/npc/metrics"
	"github.com/weaveworks/weave/npc/ulogd"
)

var (
	version     = "unreleased"
	metricsAddr string
	logLevel    string
	allowMcast  bool
	nodeName    string
	legacy      bool
	maxList     int
)

func handleError(err error) { common.CheckFatal(err) }

func makeController(getter cache.Getter, resource string,
	objType runtime.Object, handlers cache.ResourceEventHandlerFuncs) cache.Controller {
	listWatch := cache.NewListWatchFromClient(getter, resource, "", fields.Everything())
	_, controller := cache.NewInformer(listWatch, objType, 0, handlers)
	return controller
}

func resetIPTables(ipt *iptables.IPTables) error {
	// Flush chains first so there are no refs to extant ipsets
	if err := ipt.ClearChain(npc.TableFilter, npc.IngressChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(npc.TableFilter, npc.DefaultChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(npc.TableFilter, npc.MainChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(npc.TableFilter, npc.EgressMarkChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(npc.TableFilter, npc.EgressCustomChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(npc.TableFilter, npc.EgressDefaultChain); err != nil {
		return err
	}

	// We do not clear npc.EgressChain here because otherwise, in the case of restarting
	// weave-npc process, all egress traffic is allowed for a short period of time.
	// The chain is created in createBaseRules.

	return nil
}

func resetIPSets(ips ipset.Interface) error {
	// Remove ipsets prefixed `weave-` only.

	sets, err := ips.List(npc.IpsetNamePrefix)
	if err != nil {
		common.Log.Errorf("Failed to retrieve list of ipsets")
		return err
	}

	common.Log.Debugf("Got list of ipsets: %v", sets)

	// Must remove references to ipsets by other ipsets before they're destroyed
	for _, s := range sets {
		common.Log.Debugf("Flushing ipset '%s'", string(s))
		if err := ips.Flush(s); err != nil {
			common.Log.Errorf("Failed to flush ipset '%s'", string(s))
			return err
		}
	}

	for _, s := range sets {
		// LocalIPset might be used by WEAVE-NPC-EGRESS chain which we do not
		// flush, so we cannot destroy it.
		if s == npc.LocalIpset {
			continue
		}
		common.Log.Debugf("Destroying ipset '%s'", string(s))
		if err := ips.Destroy(s); err != nil {
			common.Log.Errorf("Failed to destroy ipset '%s'", string(s))
			return err
		}
	}

	return nil
}

func createBaseRules(ipt *iptables.IPTables, ips ipset.Interface, legacy bool) error {
	// Configure main chain static rules
	if err := ipt.Append(npc.TableFilter, npc.MainChain,
		"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return err
	}

	if allowMcast {
		if err := ipt.Append(npc.TableFilter, npc.MainChain,
			"-d", "224.0.0.0/4", "-j", "ACCEPT"); err != nil {
			return err
		}
	}

	if err := ipt.Append(npc.TableFilter, npc.MainChain,
		"-m", "state", "--state", "NEW", "-j", string(npc.DefaultChain)); err != nil {
		return err
	}

	if err := ipt.Append(npc.TableFilter, npc.MainChain,
		"-m", "state", "--state", "NEW", "-j", string(npc.IngressChain)); err != nil {
		return err
	}

	// If the destination address is not any of the local pods, let it through
	found, err := ipsetExist(ips, npc.LocalIpset)
	if err != nil {
		return err
	}
	if !found {
		if err := ips.Create(npc.LocalIpset, ipset.HashIP); err != nil {
			return err
		}
	}
	if err := ipt.Append(npc.TableFilter, npc.MainChain,
		"-m", "set", "!", "--match-set", npc.LocalIpset, "dst", "-j", "ACCEPT"); err != nil {
		return err
	}

	if !legacy {
		if err := ipt.Append(npc.TableFilter, npc.EgressMarkChain,
			"-j", "MARK", "--set-xmark", npc.EgressMark); err != nil {
			return err
		}

		ruleSpecs := [][]string{
			{"-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "RETURN"},
			{"-m", "state", "--state", "NEW", "-m", "set", "!", "--match-set", npc.LocalIpset, "src", "-j", "RETURN"},
		}
		if allowMcast {
			ruleSpecs = append(ruleSpecs, []string{"-d", "224.0.0.0/4", "-j", "RETURN"})
		}
		ruleSpecs = append(ruleSpecs, [][]string{
			{"-m", "state", "--state", "NEW", "-j", string(npc.EgressDefaultChain)},
			{"-m", "state", "--state", "NEW", "-m", "mark", "!", "--mark", npc.EgressMark, "-j", string(npc.EgressCustomChain)},
			{"-m", "state", "--state", "NEW", "-m", "mark", "!", "--mark", npc.EgressMark, "-j", "NFLOG", "--nflog-group", "86"},
			{"-m", "mark", "!", "--mark", npc.EgressMark, "-j", "DROP"},
		}...)
		if err := addChainWithRules(ipt, npc.TableFilter, npc.EgressChain, ruleSpecs); err != nil {
			return err
		}
	}

	return nil
}

// addChainWithRules creates a chain and appends given rules to it.
//
// If the chain exists, but its rules are not the same as the given ones, the
// function will flush the chain and then will append the rules.
func addChainWithRules(ipt *iptables.IPTables, table, chain string, rulespecs [][]string) error {
	// Create the chain ignoring any error because the chain might already exist
	_ = ipt.NewChain(table, chain)

	currRuleSpecs, err := ipt.List(table, chain)
	if err != nil {
		return errors.Wrapf(err, "iptables -S. table: %q, chain: %q", table, chain)
	}

	// First returned rule is "-N $(chain)", so ignore it
	currRules := strings.Join(currRuleSpecs[1:], "\n")
	rules := make([]string, 0)
	for _, r := range rulespecs {
		rules = append(rules, strings.Join(r, " "))
	}
	reqRules := strings.Join(rules, "\n")

	if currRules == reqRules {
		return nil
	}

	for _, r := range rulespecs {
		if err := ipt.Append(table, chain, r...); err != nil {
			return errors.Wrapf(err, "iptables -A. table: %q, chain: %q, rule: %s", table, chain, r)
		}
	}

	return nil
}

// Dummy way to check whether a given ipset exists.
// TODO(brb) Use "ipset -exist create <..>" for our purpose instead (for some reasons
// creating an ipset with -exist fails).
func ipsetExist(ips ipset.Interface, name ipset.Name) (bool, error) {
	sets, err := ips.List(string(name))
	if err != nil {
		return false, err
	}
	for _, s := range sets {
		if s == name {
			return true, nil
		}
	}
	return false, nil
}

func root(cmd *cobra.Command, args []string) {
	var npController cache.Controller

	common.SetLogLevel(logLevel)
	if nodeName == "" {
		// HOSTNAME is set by Kubernetes for pods in the host network namespace
		nodeName = os.Getenv("HOSTNAME")
	}
	if nodeName == "" {
		common.Log.Fatalf("Must set node name via --node-name or $HOSTNAME")
	}
	common.Log.Infof("Starting Weaveworks NPC %s; node name %q", version, nodeName)

	if legacy {
		common.Log.Info("Running in legacy mode (k8s pre-1.7 network policy semantics)")
	}

	if err := metrics.Start(metricsAddr); err != nil {
		common.Log.Fatalf("Failed to start metrics: %v", err)
	}

	if err := ulogd.Start(); err != nil {
		common.Log.Fatalf("Failed to start ulogd: %v", err)
	}

	config, err := rest.InClusterConfig()
	handleError(err)

	client, err := kubernetes.NewForConfig(config)
	handleError(err)

	ipt, err := iptables.New()
	handleError(err)

	ips := ipset.New(common.LogLogger(), maxList)

	handleError(resetIPTables(ipt))
	handleError(resetIPSets(ips))
	handleError(createBaseRules(ipt, ips, legacy))

	npc := npc.New(nodeName, legacy, ipt, ips)

	nsController := makeController(client.Core().RESTClient(), "namespaces", &coreapi.Namespace{},
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				handleError(npc.AddNamespace(obj.(*coreapi.Namespace)))
			},
			DeleteFunc: func(obj interface{}) {
				switch obj := obj.(type) {
				case *coreapi.Namespace:
					handleError(npc.DeleteNamespace(obj))
				case cache.DeletedFinalStateUnknown:
					// We know this object has gone away, but its final state is no longer
					// available from the API server. Instead we use the last copy of it
					// that we have, which is good enough for our cleanup.
					handleError(npc.DeleteNamespace(obj.Obj.(*coreapi.Namespace)))
				}
			},
			UpdateFunc: func(old, new interface{}) {
				handleError(npc.UpdateNamespace(old.(*coreapi.Namespace), new.(*coreapi.Namespace)))
			}})

	podController := makeController(client.Core().RESTClient(), "pods", &coreapi.Pod{},
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				handleError(npc.AddPod(obj.(*coreapi.Pod)))
			},
			DeleteFunc: func(obj interface{}) {
				switch obj := obj.(type) {
				case *coreapi.Pod:
					handleError(npc.DeletePod(obj))
				case cache.DeletedFinalStateUnknown:
					// We know this object has gone away, but its final state is no longer
					// available from the API server. Instead we use the last copy of it
					// that we have, which is good enough for our cleanup.
					handleError(npc.DeletePod(obj.Obj.(*coreapi.Pod)))
				}
			},
			UpdateFunc: func(old, new interface{}) {
				handleError(npc.UpdatePod(old.(*coreapi.Pod), new.(*coreapi.Pod)))
			}})

	npHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			handleError(npc.AddNetworkPolicy(obj))
		},
		DeleteFunc: func(obj interface{}) {
			switch obj := obj.(type) {
			case cache.DeletedFinalStateUnknown:
				// We know this object has gone away, but its final state is no longer
				// available from the API server. Instead we use the last copy of it
				// that we have, which is good enough for our cleanup.
				handleError(npc.DeleteNetworkPolicy(obj.Obj))
			default:
				handleError(npc.DeleteNetworkPolicy(obj))
			}
		},
		UpdateFunc: func(old, new interface{}) {
			handleError(npc.UpdateNetworkPolicy(old, new))
		},
	}
	if legacy {
		npController = makeController(client.Extensions().RESTClient(), "networkpolicies", &extnapi.NetworkPolicy{}, npHandlers)
	} else {
		npController = makeController(client.NetworkingV1().RESTClient(), "networkpolicies", &networkingv1.NetworkPolicy{}, npHandlers)
	}

	go nsController.Run(wait.NeverStop)
	go podController.Run(wait.NeverStop)
	go npController.Run(wait.NeverStop)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	common.Log.Fatalf("Exiting: %v", <-signals)
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "weave-npc",
		Short: "Weaveworks Kubernetes Network Policy Controller",
		Run:   root}

	rootCmd.PersistentFlags().StringVar(&metricsAddr, "metrics-addr", ":6781", "metrics server bind address")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "debug", "logging level (debug, info, warning, error)")
	rootCmd.PersistentFlags().BoolVar(&allowMcast, "allow-mcast", true, "allow all multicast traffic")
	rootCmd.PersistentFlags().StringVar(&nodeName, "node-name", "", "only generate rules that apply to this node")
	rootCmd.PersistentFlags().BoolVar(&legacy, "use-legacy-netpol", false, "use legacy network policies (pre k8s 1.7 vsn)")
	rootCmd.PersistentFlags().IntVar(&maxList, "max-list-size", 1024, "maximum size of ipset list (for namespaces)")

	handleError(rootCmd.Execute())
}
