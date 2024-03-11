/*
Copyright 2021 The Everoute Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package policy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/everoute/everoute/pkg/agent/controller/policy"
	"github.com/everoute/everoute/pkg/agent/datapath"
	clientsetscheme "github.com/everoute/everoute/pkg/client/clientset_generated/clientset/scheme"
	"github.com/everoute/everoute/pkg/types"
	"github.com/everoute/everoute/plugin/tower/pkg/informer"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	k8sClient             client.Client // You'll be using this client in your tests.
	testEnv               *envtest.Environment
	ruleCacheLister       informer.Lister
	globalRuleCacheLister informer.Lister
	useExistingCluster    bool
	ctx, cancel           = context.WithCancel(ctrl.SetupSignalHandler())
)

const (
	RunTestWithExistingCluster = "TESTING_WITH_EXISTING_CLUSTER"
	brName                     = "bridgeUT"
)

func TestPolicyController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PolicyController Suite")
}

var _ = BeforeSuite(func() {
	if os.Getenv(RunTestWithExistingCluster) == "true" {
		By("testing with existing cluster")
		useExistingCluster = true
	}
	// Wait for policyrule test initialize DpManager, and then start flow relay test, avoid connection reset error
	time.Sleep(time.Second * 30)
	/*
		First, the envtest cluster is configured to read CRDs from the CRD directory Kubebuilder scaffolds for you.
	*/
	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		UseExistingCluster: &useExistingCluster,
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths:           []string{filepath.Join("..", "..", "..", "..", "deploy", "chart", "templates", "crds")},
			CleanUpAfterUse: true,
		},
	}

	/*
		Then, we start the envtest cluster.
	*/
	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	/*
		The autogenerated test code will add schema to the default client-go k8s scheme.
	*/
	err = clientsetscheme.AddToScheme(scheme.Scheme)
	Expect(err).Should(Succeed())

	/*
		One thing that this autogenerated file is missing, however, is a way to actually start your controller.
		The code above will set up a client for interacting with your custom Kind,
		but will not be able to test your controller behavior.
		If you want to test your custom controller logic, you’ll need to add some familiar-looking manager logic
		to your BeforeSuite() function, so you can register your custom controller to run on this test cluster.
		You may notice that the code below runs your controller with nearly identical logic to your CronJob project’s main.go!
		The only difference is that the manager is started in a separate goroutine so it does not block the cleanup of envtest
		when you’re done running your tests.
		Once you've added the code below, you can actually delete the k8sClient above, because you can get k8sClient from the manager
		(as shown below).
	*/

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		// disable metrics serving
		MetricsBindAddress: "0",
	})
	Expect(err).ToNot(HaveOccurred())
	Expect(k8sManager).ToNot(BeNil())

	Expect(datapath.ExcuteCommand(datapath.SetupBridgeChain, brName)).ToNot(HaveOccurred())

	updateChan := make(chan *types.EndpointIP, 10)
	datapathManager := datapath.NewDatapathManager(&datapath.DpManagerConfig{
		ManagedVDSMap: map[string]string{
			brName: brName,
		}}, updateChan)
	datapathManager.InitializeDatapath(ctx.Done())

	policyController := &policy.Reconciler{
		Client:          k8sManager.GetClient(),
		Scheme:          k8sManager.GetScheme(),
		DatapathManager: datapathManager,
	}
	err = (policyController).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	ruleCacheLister = policyController.GetCompleteRuleLister()
	Expect(ruleCacheLister).ShouldNot(BeNil())

	globalRuleCacheLister = policyController.GetGlobalRuleLister()
	Expect(globalRuleCacheLister).ShouldNot(BeNil())

	go func() {
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred())
	}()

	k8sClient = k8sManager.GetClient()
	Expect(k8sClient).ToNot(BeNil())
	Expect(k8sManager.GetCache().WaitForCacheSync(ctx)).Should(BeTrue())
}, 60)

var _ = AfterSuite(func() {
	By("stop controller manager")
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
	Expect(datapath.ExcuteCommand(datapath.CleanBridgeChain, brName)).NotTo(HaveOccurred())

})
