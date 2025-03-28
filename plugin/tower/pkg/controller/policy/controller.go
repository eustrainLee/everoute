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

package policy

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/mikioh/ipaddr"
	"github.com/samber/lo"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nameutil "github.com/everoute/everoute/pkg/agent/controller/policy/cache"
	"github.com/everoute/everoute/pkg/apis/security/v1alpha1"
	"github.com/everoute/everoute/pkg/client/clientset_generated/clientset"
	crd "github.com/everoute/everoute/pkg/client/informers_generated/externalversions"
	"github.com/everoute/everoute/pkg/constants"
	msconst "github.com/everoute/everoute/pkg/constants/ms"
	"github.com/everoute/everoute/pkg/labels"
	"github.com/everoute/everoute/pkg/utils"
	"github.com/everoute/everoute/plugin/tower/pkg/controller/endpoint"
	"github.com/everoute/everoute/plugin/tower/pkg/informer"
	"github.com/everoute/everoute/plugin/tower/pkg/schema"
)

const (
	SecurityPolicyPrefix             = "tower.sp-"
	IsolationPolicyPrefix            = "tower.ip-"
	IsolationPolicyIngressPrefix     = "tower.ip.ingress-"
	IsolationPolicyEgressPrefix      = "tower.ip.egress-"
	SecurityPolicyCommunicablePrefix = "tower.sp.communicable-"

	SystemEndpointsPolicyName = "tower.sp.internal-system.endpoints"
	ControllerPolicyName      = "tower.sp.internal-controller"
	GlobalWhitelistPolicyName = "tower.sp.global-user.whitelist"

	FTPPortRange  = "21"
	TFTPPortRange = "69"

	InternalAllowlistPriority int32 = 90
	BlocklistPriority         int32 = 50
	AllowlistPriority         int32 = 30

	vmIndex              = "vmIndex"
	labelIndex           = "labelIndex"
	securityGroupIndex   = "securityGroupIndex"
	securityPolicyIndex  = "towerSecurityPolicyIndex"
	isolationPolicyIndex = "towerIsolationPolicyIndex"
	serviceIndex         = "serviceIndex"

	K8sNsNameLabel = "kubernetes.io/metadata.name"
)

// Controller sync SecurityPolicy and IsolationPolicy as v1alpha1.SecurityPolicy
// from tower. For v1alpha1.SecurityPolicy, has the following naming rules:
//  1. If origin policy is SecurityPolicy, policy.name = {{SecurityPolicyPrefix}}{{SecurityPolicy.ID}}
//  2. If origin policy is IsolationPolicy, policy.name = {{IsolationPolicyPrefix}}{{IsolationPolicy.ID}}
//  3. If policy was generated to make intragroup communicable, policy.name = {{SecurityPolicyCommunicablePrefix}}{{SelectorHash}}-{{SecurityPolicy.ID}}
//  4. If origin policy is SystemEndpointsPolicy, policy.name = {{SystemEndpointsPolicyName}}
//  5. If origin policy is ControllerPolicy, policy.name = {{ControllerPolicyName}}
type Controller struct {
	// name of this controller
	name string

	// namespace which endpoint and security policy should create in
	namespace    string
	podNamespace string
	// everouteCluster which should synchronize SecurityPolicy from
	everouteCluster string

	crdClient clientset.Interface

	vmInformer       cache.SharedIndexInformer
	vmLister         informer.Lister
	vmInformerSynced cache.InformerSynced

	labelInformer       cache.SharedIndexInformer
	labelLister         informer.Lister
	labelInformerSynced cache.InformerSynced

	securityPolicyInformer       cache.SharedIndexInformer
	securityPolicyLister         informer.Lister
	securityPolicyInformerSynced cache.InformerSynced

	isolationPolicyInformer       cache.SharedIndexInformer
	isolationPolicyLister         informer.Lister
	isolationPolicyInformerSynced cache.InformerSynced

	crdPolicyInformer       cache.SharedIndexInformer
	crdPolicyLister         informer.Lister
	crdPolicyInformerSynced cache.InformerSynced

	everouteClusterInformer       cache.SharedIndexInformer
	everouteClusterLister         informer.Lister
	everouteClusterInformerSynced cache.InformerSynced

	systemEndpointInformer       cache.SharedIndexInformer
	systemEndpointLister         informer.Lister
	systemEndpointInformerSynced cache.InformerSynced

	securityGroupInformer       cache.SharedIndexInformer
	securityGroupLister         informer.Lister
	securityGroupInformerSynced cache.InformerSynced

	isolationPolicyQueue       workqueue.RateLimitingInterface
	securityPolicyQueue        workqueue.RateLimitingInterface
	systemEndpointPolicyQueue  workqueue.RateLimitingInterface
	everouteClusterPolicyQueue workqueue.RateLimitingInterface

	serviceInformer       cache.SharedIndexInformer
	serviceLister         informer.Lister
	serviceInformerSynced cache.InformerSynced
}

// New creates a new instance of controller.
//
//nolint:funlen
func New(
	towerFactory informer.SharedInformerFactory,
	crdFactory crd.SharedInformerFactory,
	crdClient clientset.Interface,
	resyncPeriod time.Duration,
	namespace string,
	podNamespace string,
	everouteCluster string,
) *Controller {
	crdPolicyInformer := crdFactory.Security().V1alpha1().SecurityPolicies().Informer()
	vmInformer := towerFactory.VM()
	labelInformer := towerFactory.Label()
	securityPolicyInformer := towerFactory.SecurityPolicy()
	isolationPolicyInformer := towerFactory.IsolationPolicy()
	erClusterInformer := towerFactory.EverouteCluster()
	systemEndpointInformer := towerFactory.SystemEndpoints()
	securityGroupInformer := towerFactory.SecurityGroup()
	serviceInformer := towerFactory.Service()

	c := &Controller{
		name:                          "PolicyController",
		namespace:                     namespace,
		podNamespace:                  podNamespace,
		everouteCluster:               everouteCluster,
		crdClient:                     crdClient,
		vmInformer:                    vmInformer,
		vmLister:                      vmInformer.GetIndexer(),
		vmInformerSynced:              vmInformer.HasSynced,
		labelInformer:                 labelInformer,
		labelLister:                   labelInformer.GetIndexer(),
		labelInformerSynced:           labelInformer.HasSynced,
		securityPolicyInformer:        securityPolicyInformer,
		securityPolicyLister:          securityPolicyInformer.GetIndexer(),
		securityPolicyInformerSynced:  securityPolicyInformer.HasSynced,
		isolationPolicyInformer:       isolationPolicyInformer,
		isolationPolicyLister:         isolationPolicyInformer.GetIndexer(),
		isolationPolicyInformerSynced: isolationPolicyInformer.HasSynced,
		serviceInformer:               serviceInformer,
		serviceLister:                 serviceInformer.GetIndexer(),
		serviceInformerSynced:         serviceInformer.HasSynced,
		crdPolicyInformer:             crdPolicyInformer,
		crdPolicyLister:               crdPolicyInformer.GetIndexer(),
		crdPolicyInformerSynced:       crdPolicyInformer.HasSynced,
		everouteClusterInformer:       erClusterInformer,
		everouteClusterLister:         erClusterInformer.GetIndexer(),
		everouteClusterInformerSynced: erClusterInformer.HasSynced,
		systemEndpointInformer:        systemEndpointInformer,
		systemEndpointLister:          systemEndpointInformer.GetIndexer(),
		systemEndpointInformerSynced:  systemEndpointInformer.HasSynced,
		securityGroupInformer:         securityGroupInformer,
		securityGroupLister:           securityGroupInformer.GetIndexer(),
		securityGroupInformerSynced:   securityGroupInformer.HasSynced,
		isolationPolicyQueue:          workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		securityPolicyQueue:           workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		systemEndpointPolicyQueue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		everouteClusterPolicyQueue:    workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	// when vm's vnics changes, handle related IsolationPolicy and SecurityGroup
	_, _ = vmInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleVM,
			UpdateFunc: c.updateVM,
			DeleteFunc: c.handleVM,
		},
		resyncPeriod,
	)

	// when labels key/value changes, handle related SecurityPolicy, IsolationPolicy and SecurityGroup
	_, _ = labelInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleLabel,
			UpdateFunc: c.updateLabel,
			DeleteFunc: c.handleLabel,
		},
		resyncPeriod,
	)

	_, _ = securityPolicyInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleSecurityPolicy,
			UpdateFunc: c.updateSecurityPolicy,
			DeleteFunc: c.handleSecurityPolicy,
		},
		resyncPeriod,
	)

	_, _ = isolationPolicyInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleIsolationPolicy,
			UpdateFunc: c.updateIsolationPolicy,
			DeleteFunc: c.handleIsolationPolicy,
		},
		resyncPeriod,
	)

	_, _ = serviceInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleService,
			UpdateFunc: c.updateService,
			DeleteFunc: c.handleService,
		},
		resyncPeriod,
	)

	_, _ = erClusterInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleEverouteCluster,
			UpdateFunc: c.updateEverouteCluster,
			DeleteFunc: c.handleEverouteCluster,
		},
		resyncPeriod,
	)

	_, _ = systemEndpointInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleSystemEndpoints,
			UpdateFunc: c.updateSystemEndpoints,
			DeleteFunc: c.handleSystemEndpoints,
		},
		resyncPeriod,
	)

	// when policy changes, enqueue related SecurityPolicy and IsolationPolicy
	_, _ = crdPolicyInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleCRDPolicy,
			UpdateFunc: c.updateCRDPolicy,
			DeleteFunc: c.handleCRDPolicy,
		},
		resyncPeriod,
	)

	_, _ = securityGroupInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.handleSecurityGroup,
			UpdateFunc: c.updateSecurityGroup,
			DeleteFunc: c.handleSecurityGroup,
		},
		resyncPeriod,
	)

	// relate selected labels and security groups
	_ = securityPolicyInformer.AddIndexers(cache.Indexers{
		labelIndex:         c.labelIndexFunc,
		securityGroupIndex: c.securityGroupIndexFunc,
		serviceIndex:       c.serviceIndexFunc,
	})

	// relate isolate vm, selected labels and security groups
	_ = isolationPolicyInformer.AddIndexers(cache.Indexers{
		vmIndex:            c.vmIndexFunc,
		labelIndex:         c.labelIndexFunc,
		securityGroupIndex: c.securityGroupIndexFunc,
		serviceIndex:       c.serviceIndexFunc,
	})

	// relate vms and selected labels
	_ = securityGroupInformer.AddIndexers(cache.Indexers{
		vmIndex:    c.vmIndexFunc,
		labelIndex: c.labelIndexFunc,
	})

	// relate vms
	_ = systemEndpointInformer.AddIndexers(cache.Indexers{
		vmIndex: c.vmIndexFunc,
	})

	// relate owner SecurityPolicy or IsolationPolicy
	_ = crdPolicyInformer.AddIndexers(cache.Indexers{
		securityPolicyIndex:  c.securityPolicyIndexFunc,
		isolationPolicyIndex: c.isolationPolicyIndexFunc,
	})

	_ = erClusterInformer.AddIndexers(cache.Indexers{
		serviceIndex: c.serviceIndexFunc,
	})

	return c
}

// Run begins processing items, and will continue until a value is sent down stopCh, or stopCh closed.
func (c *Controller) Run(workers uint, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.securityPolicyQueue.ShutDown()
	defer c.isolationPolicyQueue.ShutDown()
	defer c.systemEndpointPolicyQueue.ShutDown()
	defer c.everouteClusterPolicyQueue.ShutDown()

	if !cache.WaitForNamedCacheSync(c.name, stopCh,
		c.vmInformerSynced,
		c.labelInformerSynced,
		c.securityPolicyInformerSynced,
		c.isolationPolicyInformerSynced,
		c.crdPolicyInformerSynced,
		c.everouteClusterInformerSynced,
		c.systemEndpointInformerSynced,
		c.securityGroupInformerSynced,
		c.serviceInformerSynced,
	) {
		return
	}

	for i := uint(0); i < workers; i++ {
		go wait.Until(informer.ReconcileWorker(c.name, c.securityPolicyQueue, c.syncSecurityPolicy), time.Second, stopCh)
		go wait.Until(informer.ReconcileWorker(c.name, c.isolationPolicyQueue, c.syncIsolationPolicy), time.Second, stopCh)
	}
	// handle systemendpoints and everoutecluster separately
	go wait.Until(informer.ReconcileWorker(c.name, c.everouteClusterPolicyQueue, c.syncEverouteClusterPolicy), time.Second, stopCh)
	go wait.Until(informer.ReconcileWorker(c.name, c.systemEndpointPolicyQueue, c.syncSystemEndpointsPolicy), time.Second, stopCh)

	<-stopCh
}

func (c *Controller) labelIndexFunc(obj interface{}) ([]string, error) {
	var labelReferences []schema.ObjectReference

	switch o := obj.(type) {
	case *schema.SecurityPolicy:
		for _, peer := range o.ApplyTo {
			labelReferences = append(labelReferences, peer.Selector...)
		}
		for _, peer := range append(o.Ingress, o.Egress...) {
			labelReferences = append(labelReferences, peer.Selector...)
		}
	case *schema.IsolationPolicy:
		for _, peer := range append(o.Ingress, o.Egress...) {
			labelReferences = append(labelReferences, peer.Selector...)
		}
	case *schema.SecurityGroup:
		for _, peer := range o.LabelGroups {
			labelReferences = append(labelReferences, peer.Labels...)
		}
	}

	labelKeys := make([]string, len(labelReferences))
	for _, labelReference := range labelReferences {
		labelKeys = append(labelKeys, labelReference.ID)
	}

	return labelKeys, nil
}

func (c *Controller) vmIndexFunc(obj interface{}) ([]string, error) {
	var vms []string

	switch o := obj.(type) {
	case *schema.IsolationPolicy:
		vms = []string{o.VM.ID}
	case *schema.SecurityGroup:
		for _, vm := range o.VMs {
			vms = append(vms, vm.ID)
		}
	case *schema.SystemEndpoints:
		for _, vm := range o.IDEndpoints {
			vms = append(vms, vm.VMID)
		}
	}

	return vms, nil
}

func (c *Controller) securityGroupIndexFunc(obj interface{}) ([]string, error) {
	var securityGroups []string

	switch o := obj.(type) {
	case *schema.SecurityPolicy:
		for _, peer := range o.ApplyTo {
			if peer.SecurityGroup != nil {
				securityGroups = append(securityGroups, peer.SecurityGroup.ID)
			}
		}
		for _, peer := range append(o.Ingress, o.Egress...) {
			if peer.SecurityGroup != nil {
				securityGroups = append(securityGroups, peer.SecurityGroup.ID)
			}
		}
	case *schema.IsolationPolicy:
		for _, peer := range append(o.Ingress, o.Egress...) {
			if peer.SecurityGroup != nil {
				securityGroups = append(securityGroups, peer.SecurityGroup.ID)
			}
		}
	}

	return securityGroups, nil
}

func (c *Controller) serviceIndexFunc(obj interface{}) ([]string, error) {
	var serviceIDs []string

	switch o := obj.(type) {
	case *schema.SecurityPolicy:
		for _, rule := range append(o.Ingress, o.Egress...) {
			for _, s := range rule.Services {
				serviceIDs = append(serviceIDs, s.ID)
			}
		}
	case *schema.IsolationPolicy:
		for _, rule := range append(o.Ingress, o.Egress...) {
			for _, s := range rule.Services {
				serviceIDs = append(serviceIDs, s.ID)
			}
		}
	case *schema.EverouteCluster:
		for _, rule := range append(o.GlobalWhitelist.Ingress, o.GlobalWhitelist.Egress...) {
			for _, s := range rule.Services {
				serviceIDs = append(serviceIDs, s.ID)
			}
		}
	}
	return serviceIDs, nil
}

func (c *Controller) skipByNamespace(obj metav1.Object) bool {
	return !(obj.GetNamespace() == c.namespace || obj.GetNamespace() == c.podNamespace)
}

func (c *Controller) securityPolicyIndexFunc(obj interface{}) ([]string, error) {
	policy := obj.(*v1alpha1.SecurityPolicy)
	if c.skipByNamespace(policy) {
		return nil, nil
	}
	if strings.HasPrefix(policy.GetName(), SecurityPolicyPrefix) {
		securityPolicyKey := strings.TrimPrefix(policy.GetName(), SecurityPolicyPrefix)
		return []string{securityPolicyKey}, nil
	}

	if strings.HasPrefix(policy.GetName(), SecurityPolicyCommunicablePrefix) {
		withoutPrefix := strings.TrimPrefix(policy.GetName(), SecurityPolicyCommunicablePrefix)
		securityPolicyKey := strings.Split(withoutPrefix, "-")[1]
		return []string{securityPolicyKey}, nil
	}

	return nil, nil
}

func (c *Controller) isolationPolicyIndexFunc(obj interface{}) ([]string, error) {
	policy := obj.(*v1alpha1.SecurityPolicy)

	if c.skipByNamespace(policy) {
		return nil, nil
	}
	if strings.HasPrefix(policy.GetName(), strings.TrimSuffix(IsolationPolicyPrefix, "-")) {
		if strings.HasPrefix(policy.GetName(), IsolationPolicyIngressPrefix) {
			return []string{strings.TrimPrefix(policy.GetName(), IsolationPolicyIngressPrefix)}, nil
		}
		if strings.HasPrefix(policy.GetName(), IsolationPolicyEgressPrefix) {
			return []string{strings.TrimPrefix(policy.GetName(), IsolationPolicyEgressPrefix)}, nil
		}
		return []string{strings.TrimPrefix(policy.GetName(), IsolationPolicyPrefix)}, nil
	}

	return nil, nil
}

func (c *Controller) handleVM(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}

	policies, _ := c.isolationPolicyLister.ByIndex(vmIndex, obj.(*schema.VM).GetID())
	for _, policy := range policies {
		c.handleIsolationPolicy(policy)
	}

	systemEndpoints, _ := c.systemEndpointLister.ByIndex(vmIndex, obj.(*schema.VM).GetID())
	if len(systemEndpoints) != 0 {
		c.handleSystemEndpoints(nil)
	}

	securityGroups, _ := c.securityGroupLister.ByIndex(vmIndex, obj.(*schema.VM).GetID())
	for _, group := range securityGroups {
		c.handleSecurityGroup(group)
	}
}

func (c *Controller) updateVM(old, new interface{}) {
	oldVM := old.(*schema.VM)
	newVM := new.(*schema.VM)

	if reflect.DeepEqual(newVM.VMNics, oldVM.VMNics) {
		return
	}
	c.handleVM(newVM)
}

func (c *Controller) handleLabel(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}

	securityPolicies, _ := c.securityPolicyLister.ByIndex(labelIndex, obj.(*schema.Label).GetID())
	for _, securityPolicy := range securityPolicies {
		c.handleSecurityPolicy(securityPolicy)
	}

	isolationPolicies, _ := c.isolationPolicyLister.ByIndex(labelIndex, obj.(*schema.Label).GetID())
	for _, isolationPolicy := range isolationPolicies {
		c.handleIsolationPolicy(isolationPolicy)
	}

	securityGroups, _ := c.securityGroupLister.ByIndex(labelIndex, obj.(*schema.Label).GetID())
	for _, securityGroup := range securityGroups {
		c.handleSecurityGroup(securityGroup)
	}
}

func (c *Controller) updateLabel(old, new interface{}) {
	oldLabel := old.(*schema.Label)
	newLabel := new.(*schema.Label)

	if oldLabel.Key == newLabel.Key && oldLabel.Value == newLabel.Value {
		return
	}
	c.handleLabel(newLabel)
}

func (c *Controller) handleSecurityPolicy(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}
	policy := obj.(*schema.SecurityPolicy)
	// when policy delete, policy.EverouteCluster.ID would be empty
	if policy.EverouteCluster.ID == "" || policy.EverouteCluster.ID == c.everouteCluster {
		c.securityPolicyQueue.Add(policy.GetID())
	}
}

func (c *Controller) updateSecurityPolicy(old, new interface{}) {
	oldPolicy := old.(*schema.SecurityPolicy)
	newPolicy := new.(*schema.SecurityPolicy)

	if reflect.DeepEqual(newPolicy, oldPolicy) {
		return
	}
	c.handleSecurityPolicy(newPolicy)
}

func (c *Controller) handleIsolationPolicy(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}
	policy := obj.(*schema.IsolationPolicy)
	// when policy delete, policy.EverouteCluster.ID would be empty
	if policy.EverouteCluster.ID == "" || policy.EverouteCluster.ID == c.everouteCluster {
		c.isolationPolicyQueue.Add(policy.GetID())
	}
}

func (c *Controller) updateIsolationPolicy(old, new interface{}) {
	oldPolicy := old.(*schema.IsolationPolicy)
	newPolicy := new.(*schema.IsolationPolicy)

	if reflect.DeepEqual(newPolicy, oldPolicy) {
		return
	}
	c.handleIsolationPolicy(newPolicy)
}

func (c *Controller) handleCRDPolicy(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}

	securityPolicies, _ := c.securityPolicyIndexFunc(obj)
	for _, policy := range securityPolicies {
		c.securityPolicyQueue.Add(policy)
	}

	isolationPolicies, _ := c.isolationPolicyIndexFunc(obj)
	for _, policy := range isolationPolicies {
		c.isolationPolicyQueue.Add(policy)
	}

	if obj.(*v1alpha1.SecurityPolicy).Name == SystemEndpointsPolicyName {
		c.handleSystemEndpoints(nil)
	}

	if obj.(*v1alpha1.SecurityPolicy).Name == ControllerPolicyName ||
		obj.(*v1alpha1.SecurityPolicy).Name == GlobalWhitelistPolicyName {
		c.handleEverouteCluster(nil)
	}
}

func (c *Controller) updateCRDPolicy(old, new interface{}) {
	oldPolicy := old.(*v1alpha1.SecurityPolicy)
	newPolicy := new.(*v1alpha1.SecurityPolicy)

	if reflect.DeepEqual(oldPolicy, newPolicy) {
		return
	}
	c.handleCRDPolicy(newPolicy)
}

func (c *Controller) handleEverouteCluster(interface{}) {
	c.everouteClusterPolicyQueue.Add("key")
}

func (c *Controller) updateEverouteCluster(old, new interface{}) {
	oldERCluster := old.(*schema.EverouteCluster)
	newERCluster := new.(*schema.EverouteCluster)

	if newERCluster.ID == c.everouteCluster {
		c.handleEverouteCluster(newERCluster)
		return
	}

	// handle controller instance ip changes
	if !reflect.DeepEqual(newERCluster.ControllerInstances, oldERCluster.ControllerInstances) {
		c.handleEverouteCluster(newERCluster)
	}
}

func (c *Controller) handleSystemEndpoints(interface{}) {
	c.systemEndpointPolicyQueue.Add("key")
}

func (c *Controller) updateSystemEndpoints(old, new interface{}) {
	oldSystemEndpoints := old.(*schema.SystemEndpoints)
	newSystemEndpoints := new.(*schema.SystemEndpoints)

	// handle systemEndpoints IP changes
	if !reflect.DeepEqual(newSystemEndpoints, oldSystemEndpoints) {
		c.handleSystemEndpoints(newSystemEndpoints)
	}
}

func (c *Controller) handleSecurityGroup(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}

	securityGroup := obj.(*schema.SecurityGroup)

	// when security group delete, this.EverouteCluster.ID would be empty
	if securityGroup.EverouteCluster.ID != "" &&
		securityGroup.EverouteCluster.ID != c.everouteCluster {
		return
	}

	securityPolicies, _ := c.securityPolicyLister.ByIndex(securityGroupIndex, securityGroup.GetID())
	for _, securityPolicy := range securityPolicies {
		c.handleSecurityPolicy(securityPolicy)
	}

	isolationPolicies, _ := c.isolationPolicyLister.ByIndex(securityGroupIndex, securityGroup.GetID())
	for _, isolationPolicy := range isolationPolicies {
		c.handleIsolationPolicy(isolationPolicy)
	}
}

func (c *Controller) updateSecurityGroup(old, new interface{}) {
	oldGroup := old.(*schema.SecurityGroup)
	newGroup := new.(*schema.SecurityGroup)

	if reflect.DeepEqual(newGroup, oldGroup) {
		return
	}
	c.handleSecurityGroup(newGroup)
}

func (c *Controller) handleService(obj interface{}) {
	unknow, ok := obj.(cache.DeletedFinalStateUnknown)
	if ok {
		obj = unknow.Obj
	}

	svc := obj.(*schema.NetworkPolicyRuleService)

	secPolicies, _ := c.securityPolicyLister.ByIndex(serviceIndex, svc.GetID())
	for _, p := range secPolicies {
		c.handleSecurityPolicy(p)
	}

	isoPolices, _ := c.isolationPolicyLister.ByIndex(serviceIndex, svc.GetID())
	for _, p := range isoPolices {
		c.handleIsolationPolicy(p)
	}

	ers, _ := c.everouteClusterLister.ByIndex(serviceIndex, svc.GetID())
	if len(ers) > 0 {
		c.handleEverouteCluster(ers[0])
	}
}

func (c *Controller) updateService(old, new interface{}) {
	oldSvc := old.(*schema.NetworkPolicyRuleService)
	newSvc := new.(*schema.NetworkPolicyRuleService)

	if !serviceMembersToSets(oldSvc).Equal(serviceMembersToSets(newSvc)) {
		c.handleService(newSvc)
	}
}

// syncSecurityPolicy sync SecurityPoicy to v1alpha1.SecurityPolicy
func (c *Controller) syncSecurityPolicy(key string) error {
	policy, exist, err := c.securityPolicyLister.GetByKey(key)
	if err != nil {
		klog.Errorf("get SecurityPolicy %s: %s", key, err)
		return err
	}

	if !exist {
		return c.deleteRelatedPolicies(securityPolicyIndex, key)
	}
	return c.processSecurityPolicyUpdate(policy.(*schema.SecurityPolicy))
}

// syncIsolationPolicy sync IsolationPolicy to v1alpha1.SecurityPolicy
func (c *Controller) syncIsolationPolicy(key string) error {
	policy, exist, err := c.isolationPolicyLister.GetByKey(key)
	if err != nil {
		klog.Errorf("get IsolationPolicy %s: %s", key, err)
		return err
	}

	if !exist {
		return c.deleteRelatedPolicies(isolationPolicyIndex, key)
	}
	return c.processIsolationPolicyUpdate(policy.(*schema.IsolationPolicy))
}

// syncSystemEndpointsPolicy sync SystemEndpoints to v1alpha1.SecurityPolicy
func (c *Controller) syncSystemEndpointsPolicy(key string) error {
	systemEndpointsList := c.systemEndpointLister.List()
	switch len(systemEndpointsList) {
	case 0:
		err := c.applyPoliciesChanges([]string{c.getSystemEndpointsPolicyKey()}, nil)
		if err != nil {
			klog.Errorf("unable delete systemEndpoints policies %+v: %s", key, err)
		}
		return err
	case 1:
		policy, _ := c.parseSystemEndpointsPolicy(systemEndpointsList[0].(*schema.SystemEndpoints))
		err := c.applyPoliciesChanges([]string{c.getSystemEndpointsPolicyKey()}, policy)
		if err != nil {
			klog.Errorf("unable update systemEndpoints policies %+v: %s", key, err)
		}
		return err
	default:
		return fmt.Errorf("invalid systemEndpoints in cluster, %+v", systemEndpointsList)
	}
}

// syncEverouteClusterPolicy sync EverouteCluster to v1alpha1.SecurityPolicy
func (c *Controller) syncEverouteClusterPolicy(string) error {
	clusterList := c.everouteClusterLister.List()

	var clusters []*schema.EverouteCluster
	for _, cluster := range clusterList {
		clusters = append(clusters, cluster.(*schema.EverouteCluster))
	}

	// process controller ip policy
	ctrlPolicy, _ := c.parseControllerPolicy(clusters)
	err := c.applyPoliciesChanges([]string{c.getControllerPolicyKey()}, ctrlPolicy)
	if err != nil {
		return fmt.Errorf("unable update EverouteCluster policies : %s", err)
	}

	// process user-defined global whitelist
	currentCluster, exist, err := c.everouteClusterLister.GetByKey(c.everouteCluster)
	if err != nil {
		return fmt.Errorf("get everouteClustes error: %s", err)
	}
	if !exist {
		return fmt.Errorf("everouteCluster %s not found", c.everouteCluster)
	}

	whitelistPolicy, err := c.parseGlobalWhitelistPolicy(currentCluster.(*schema.EverouteCluster))
	if err != nil {
		return fmt.Errorf("create global whitelist policy error: %s", err)
	}
	err = c.applyPoliciesChanges([]string{c.getGlobalWhitelistPolicyKey()}, whitelistPolicy)
	if err != nil {
		return fmt.Errorf("unable update EverouteCluster policies: %s", err)
	}

	return nil
}

func (c *Controller) deleteRelatedPolicies(indexName, key string) error {
	policyKeys, err := c.crdPolicyLister.IndexKeys(indexName, key)
	if err != nil {
		klog.Errorf("list index %s=%s related policies: %s", indexName, key, err)
		return err
	}

	err = c.applyPoliciesChanges(policyKeys, nil)
	if err != nil {
		klog.Errorf("unable delete policies %+v: %s", policyKeys, err)
		return err
	}

	return nil
}

func (c *Controller) processSecurityPolicyUpdate(policy *schema.SecurityPolicy) error {
	policies, err := c.parseSecurityPolicy(policy)
	if err != nil {
		klog.Errorf("parse SecurityPolicy %+v to []v1alpha1.SecurityPolicy: %s", policy, err)
		return err
	}

	currentPolicyKeys, err := c.crdPolicyLister.IndexKeys(securityPolicyIndex, policy.GetID())
	if err != nil {
		klog.Errorf("list v1alpha1.SecurityPolicies: %s", err)
		return err
	}

	err = c.applyPoliciesChanges(currentPolicyKeys, policies)
	if err != nil {
		klog.Errorf("unable sync SecurityPolicies %+v: %s", policies, err)
		return err
	}

	return nil
}

func (c *Controller) processIsolationPolicyUpdate(policy *schema.IsolationPolicy) error {
	policies, err := c.parseIsolationPolicy(policy)
	if err != nil {
		klog.Errorf("parse IsolationPolicy %+v to []v1alpha1.SecurityPolicy: %s", policy, err)
		return err
	}

	currentPolicyKeys, err := c.crdPolicyLister.IndexKeys(isolationPolicyIndex, policy.GetID())
	if err != nil {
		klog.Errorf("list v1alpha1.SecurityPolicies: %s", err)
		return err
	}

	err = c.applyPoliciesChanges(currentPolicyKeys, policies)
	if err != nil {
		klog.Errorf("unable apply policies %+v: %s", policies, err)
		return err
	}

	return nil
}

func (c *Controller) applyPoliciesChanges(oldKeys []string, new []v1alpha1.SecurityPolicy) error {
	oldKeySet := sets.NewString(oldKeys...)

	for _, policy := range new {
		policyKey, _ := cache.MetaNamespaceKeyFunc(policy.DeepCopy())
		if oldKeySet.Has(policyKey) {
			obj, exist, err := c.crdPolicyLister.GetByKey(policyKey)
			if err != nil {
				return fmt.Errorf("get policy %s: %s", policyKey, err)
			}
			oldKeySet.Delete(policyKey)
			if exist {
				// update the policy
				oldPolicyMeta := obj.(*v1alpha1.SecurityPolicy).ObjectMeta
				policy.ObjectMeta = oldPolicyMeta
				if reflect.DeepEqual(policy.Spec, obj.(*v1alpha1.SecurityPolicy).Spec) {
					// ignore update if old and new are same
					continue
				}
				_, err := c.crdClient.SecurityV1alpha1().SecurityPolicies(policy.GetNamespace()).Update(context.Background(), policy.DeepCopy(), metav1.UpdateOptions{})
				if err != nil {
					return fmt.Errorf("update policy %+v: %s", policy, err)
				}
				klog.Infof("update policy %s: %+v", policyKey, policy)
				continue
			}
			// if not exist, create the policy
		}

		// create the policy
		_, err := c.crdClient.SecurityV1alpha1().SecurityPolicies(policy.GetNamespace()).Create(context.Background(), policy.DeepCopy(), metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create policy %+v: %s", policy, err)
		}
		if err == nil {
			klog.Infof("create policy %s: %+v", policyKey, policy)
		}
	}

	for _, policyKey := range oldKeySet.List() {
		namespace, name, _ := cache.SplitMetaNamespaceKey(policyKey)
		err := c.crdClient.SecurityV1alpha1().SecurityPolicies(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete policy %s: %s", policyKey, err)
		}
		if err == nil {
			klog.Infof("delete policy %s", policyKey)
		}
	}

	return nil
}

// parseGlobalWhitelistPolicy convert schema.EverouteCluster Whitelist to []v1alpha1.SecurityPolicy
func (c *Controller) parseGlobalWhitelistPolicy(cluster *schema.EverouteCluster) ([]v1alpha1.SecurityPolicy, error) {
	if len(cluster.GlobalWhitelist.Ingress) == 0 && len(cluster.GlobalWhitelist.Egress) == 0 {
		return nil, nil
	}

	ingress, egress, err := c.parseNetworkPolicyRules(cluster.GlobalWhitelist.Ingress, cluster.GlobalWhitelist.Egress)
	if err != nil {
		return nil, fmt.Errorf("parse NetworkPolicyRules error, err: %s", err)
	}

	sp := v1alpha1.SecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GlobalWhitelistPolicyName,
			Namespace: c.namespace,
		},
		Spec: v1alpha1.SecurityPolicySpec{
			Priority:                      AllowlistPriority,
			SecurityPolicyEnforcementMode: getGlobalWhitelistPolicyEnforceMode(cluster.GlobalWhitelist.Enable),
			Tier:                          constants.Tier2,
			DefaultRule:                   v1alpha1.DefaultRuleNone,
			IngressRules:                  ingress,
			EgressRules:                   egress,
			Logging:                       NewLoggingOptionsFrom(cluster, c.vmLister),
			PolicyTypes:                   []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}

	return []v1alpha1.SecurityPolicy{sp}, nil
}

// parseControllerPolicy convert schema.EverouteCluster Controller to []v1alpha1.SecurityPolicy
func (c *Controller) parseControllerPolicy(clusters []*schema.EverouteCluster) ([]v1alpha1.SecurityPolicy, error) {
	sp := v1alpha1.SecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ControllerPolicyName,
			Namespace: c.namespace,
		},
		Spec: v1alpha1.SecurityPolicySpec{
			Tier:         constants.Tier2,
			Priority:     InternalAllowlistPriority,
			DefaultRule:  v1alpha1.DefaultRuleNone,
			IngressRules: []v1alpha1.Rule{{Name: "ingress"}},
			EgressRules:  []v1alpha1.Rule{{Name: "egress"}},
			PolicyTypes:  []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	for _, cluster := range clusters {
		for _, ctrl := range cluster.ControllerInstances {
			epName := endpoint.GetCtrlEndpointName(cluster.ID, ctrl)
			sp.Spec.AppliedTo = append(sp.Spec.AppliedTo, v1alpha1.ApplyToPeer{
				Endpoint: &epName,
			})
		}
	}
	if len(sp.Spec.AppliedTo) == 0 {
		return nil, nil
	}

	return []v1alpha1.SecurityPolicy{sp}, nil
}

// parseSystemEndpointsPolicy convert schema.SystemEndpoints to []v1alpha1.SecurityPolicy
func (c *Controller) parseSystemEndpointsPolicy(systemEndpoints *schema.SystemEndpoints) ([]v1alpha1.SecurityPolicy, error) {
	sp := v1alpha1.SecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SystemEndpointsPolicyName,
			Namespace: c.namespace,
		},
		Spec: v1alpha1.SecurityPolicySpec{
			Tier:         constants.Tier2,
			Priority:     InternalAllowlistPriority,
			DefaultRule:  v1alpha1.DefaultRuleNone,
			IngressRules: []v1alpha1.Rule{{Name: "ingress"}},
			EgressRules:  []v1alpha1.Rule{{Name: "egress"}},
			PolicyTypes:  []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	for _, ip := range systemEndpoints.IPPortEndpoints {
		epName := endpoint.GetSystemEndpointName(ip.Key)
		sp.Spec.AppliedTo = append(sp.Spec.AppliedTo, v1alpha1.ApplyToPeer{
			Endpoint: &epName,
		})
	}
	for _, ep := range systemEndpoints.IDEndpoints {
		applies, err := c.vmAsAppliedTo(ep.VMID)
		if err != nil {
			klog.Errorf("invalid endpoint info: %s", err)
			continue
		}
		sp.Spec.AppliedTo = append(sp.Spec.AppliedTo, applies...)
	}
	if len(sp.Spec.AppliedTo) == 0 {
		return nil, nil
	}

	return []v1alpha1.SecurityPolicy{sp}, nil
}

// parseSecurityPolicy convert schema.SecurityPolicy to []v1alpha1.SecurityPolicy
func (c *Controller) parseSecurityPolicy(securityPolicy *schema.SecurityPolicy) ([]v1alpha1.SecurityPolicy, error) {
	var policyList []v1alpha1.SecurityPolicy
	var policyMode = parseEnforcementMode(securityPolicy.PolicyMode)

	applyToVMs, applyToPods, err := c.parseSecurityPolicyApplys(securityPolicy.ApplyTo)
	if err != nil {
		return nil, err
	}
	if len(applyToVMs) == 0 && len(applyToPods) == 0 {
		return nil, nil
	}
	ingress, egress, err := c.parseNetworkPolicyRules(securityPolicy.Ingress, securityPolicy.Egress)
	if err != nil {
		return nil, err
	}
	loggingOptions := NewLoggingOptionsFrom(securityPolicy, c.vmLister)

	for _, ns := range []string{c.namespace, c.podNamespace} {
		var applyToPeers []v1alpha1.ApplyToPeer
		if ns == c.namespace {
			if len(applyToVMs) == 0 {
				continue
			}
			applyToPeers = append([]v1alpha1.ApplyToPeer{}, applyToVMs...)
		} else {
			if len(applyToPods) == 0 {
				continue
			}
			applyToPeers = append([]v1alpha1.ApplyToPeer{}, applyToPods...)
		}

		policy := v1alpha1.SecurityPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SecurityPolicyPrefix + securityPolicy.GetID(),
				Namespace: ns,
			},
			Spec: v1alpha1.SecurityPolicySpec{
				IsBlocklist:                   securityPolicy.IsBlocklist,
				Tier:                          constants.Tier2,
				Priority:                      c.getPolicyPriority(securityPolicy),
				SecurityPolicyEnforcementMode: policyMode,
				SymmetricMode:                 c.getPolicySymmetricMode(securityPolicy),
				AppliedTo:                     applyToPeers,
				IngressRules:                  ingress,
				EgressRules:                   egress,
				DefaultRule:                   c.getPolicyDefaultRule(securityPolicy),
				Logging:                       loggingOptions,
				PolicyTypes:                   []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			},
		}
		policyList = append(policyList, policy)
	}

	for item := range securityPolicy.ApplyTo {
		if !securityPolicy.ApplyTo[item].Communicable {
			continue
		}
		// generate intra group policy
		policy, err := c.generateIntragroupPolicy(securityPolicy.GetID(), policyMode, &securityPolicy.ApplyTo[item], loggingOptions)
		if err != nil {
			return nil, err
		}
		if policy != nil {
			policyList = append(policyList, *policy)
		}
	}

	return policyList, nil
}

func (c *Controller) getPolicyPriority(policy *schema.SecurityPolicy) int32 {
	if policy.IsBlocklist {
		return BlocklistPriority
	}
	return AllowlistPriority
}

func (c *Controller) getPolicyDefaultRule(policy *schema.SecurityPolicy) v1alpha1.DefaultRuleType {
	if policy.IsBlocklist {
		return v1alpha1.DefaultRuleNone
	}
	return v1alpha1.DefaultRuleDrop
}

func (c *Controller) getPolicySymmetricMode(policy *schema.SecurityPolicy) bool {
	return !policy.IsBlocklist
}

// parseIsolationPolicy convert schema.IsolationPolicy to []v1alpha1.SecurityPolicy
func (c *Controller) parseIsolationPolicy(isolationPolicy *schema.IsolationPolicy) ([]v1alpha1.SecurityPolicy, error) {
	applyToPeers, err := c.vmAsAppliedTo(isolationPolicy.VM.ID)
	if err != nil {
		return nil, err
	}
	if len(applyToPeers) == 0 {
		return nil, nil
	}

	var isolationPolices []v1alpha1.SecurityPolicy
	var loggingOptions = NewLoggingOptionsFrom(isolationPolicy, c.vmLister)

	switch isolationPolicy.Mode {
	case schema.IsolationModeAll:
		// IsolationModeAll should not create ingress or egress rule
		isolationPolices = append(isolationPolices, c.generateIsolationPolicy(
			isolationPolicy.GetID(),
			schema.IsolationModeAll,
			applyToPeers,
			nil,
			nil,
			loggingOptions,
		)...)
	case schema.IsolationModePartial:
		ingress, egress, err := c.parseNetworkPolicyRules(isolationPolicy.Ingress, isolationPolicy.Egress)
		if err != nil {
			return nil, err
		}
		isolationPolices = append(isolationPolices, c.generateIsolationPolicy(
			isolationPolicy.GetID(),
			schema.IsolationModePartial,
			applyToPeers,
			ingress,
			egress,
			loggingOptions,
		)...)
	}

	return isolationPolices, nil
}

func (c *Controller) generateIsolationPolicy(
	id string,
	mode schema.IsolationMode,
	applyToPeers []v1alpha1.ApplyToPeer,
	ingress, egress []v1alpha1.Rule,
	loggingOptions *v1alpha1.Logging,
) []v1alpha1.SecurityPolicy {
	var isolationPolices []v1alpha1.SecurityPolicy
	switch mode {
	case schema.IsolationModeAll:
		policy := v1alpha1.SecurityPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      IsolationPolicyPrefix + id,
				Namespace: c.namespace,
			},
			Spec: v1alpha1.SecurityPolicySpec{
				SymmetricMode: true,
				Tier:          constants.Tier0,
				AppliedTo:     applyToPeers,
				DefaultRule:   v1alpha1.DefaultRuleDrop,
				Logging:       loggingOptions,
				PolicyTypes:   []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			},
		}
		isolationPolices = append(isolationPolices, policy)
	case schema.IsolationModePartial:
		// separate partial policy into ingress and egress policy
		ingressPolicy := v1alpha1.SecurityPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      IsolationPolicyIngressPrefix + id,
				Namespace: c.namespace,
			},
			Spec: v1alpha1.SecurityPolicySpec{
				SymmetricMode: true,
				AppliedTo:     applyToPeers,
				DefaultRule:   v1alpha1.DefaultRuleDrop,
				Logging:       loggingOptions,
				PolicyTypes:   []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				IngressRules:  ingress,
				Tier:          constants.Tier1,
			},
		}
		if len(ingress) == 0 {
			ingressPolicy.Spec.Tier = constants.Tier0
		}
		isolationPolices = append(isolationPolices, ingressPolicy)

		egressPolicy := v1alpha1.SecurityPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      IsolationPolicyEgressPrefix + id,
				Namespace: c.namespace,
			},
			Spec: v1alpha1.SecurityPolicySpec{
				SymmetricMode: true,
				AppliedTo:     applyToPeers,
				DefaultRule:   v1alpha1.DefaultRuleDrop,
				PolicyTypes:   []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				EgressRules:   egress,
				Tier:          constants.Tier1,
				Logging:       loggingOptions,
			},
		}
		if len(egress) == 0 {
			egressPolicy.Spec.Tier = constants.Tier0
		}
		isolationPolices = append(isolationPolices, egressPolicy)
	}

	return isolationPolices
}

func (c *Controller) generateIntragroupPolicy(
	id string,
	policyMode v1alpha1.PolicyMode,
	appliedPeer *schema.SecurityPolicyApply,
	loggingOptions *v1alpha1.Logging,
) (*v1alpha1.SecurityPolicy, error) {
	peerHash := nameutil.HashName(10, appliedPeer)

	appliedPeers, _, err := c.parseSecurityPolicyApplys([]schema.SecurityPolicyApply{*appliedPeer})
	if err != nil {
		return nil, err
	}
	if len(appliedPeers) == 0 {
		return nil, nil
	}

	policy := v1alpha1.SecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecurityPolicyCommunicablePrefix + peerHash + "-" + id,
			Namespace: c.namespace,
		},
		Spec: v1alpha1.SecurityPolicySpec{
			Tier:      constants.Tier2,
			AppliedTo: appliedPeers,
			Priority:  AllowlistPriority,
			IngressRules: []v1alpha1.Rule{{
				Name: "ingress",
				From: c.appliedPeersAsPolicyPeers(appliedPeers, false, false),
			}},
			EgressRules: []v1alpha1.Rule{{
				Name: "egress",
				To:   c.appliedPeersAsPolicyPeers(appliedPeers, false, false),
			}},
			SecurityPolicyEnforcementMode: policyMode,
			DefaultRule:                   v1alpha1.DefaultRuleDrop,
			Logging:                       loggingOptions,
			PolicyTypes:                   []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}

	return &policy, nil
}

func (c *Controller) parseSecurityPolicyApplys(policyApplies []schema.SecurityPolicyApply) ([]v1alpha1.ApplyToPeer, []v1alpha1.ApplyToPeer, error) {
	var applyToVMs, applyToPods []v1alpha1.ApplyToPeer

	for _, policyApply := range policyApplies {
		switch policyApply.Type {
		case "", schema.SecurityPolicyTypeSelector:
			endpointSelector, err := c.parseSelectors(policyApply.Selector)
			if err != nil {
				return nil, nil, err
			}
			applyToVMs = append(applyToVMs, v1alpha1.ApplyToPeer{
				EndpointSelector: endpointSelector,
			})
		case schema.SecurityPolicyTypeSecurityGroup:
			if policyApply.SecurityGroup == nil {
				return nil, nil, fmt.Errorf("receive rule.Type %s but empty SecurityGroup", schema.SecurityPolicyTypeSecurityGroup)
			}
			peers, isPod, err := c.parseSecurityGroup(policyApply.SecurityGroup)
			if err != nil {
				return nil, nil, err
			}
			if isPod {
				applyToPods = append(applyToPods, peers...)
			} else {
				applyToVMs = append(applyToVMs, peers...)
			}
		default:
			return nil, nil, fmt.Errorf("unknown policy peer type %s", policyApply.Type)
		}
	}

	return applyToVMs, applyToPods, nil
}

func (c *Controller) vmAsAppliedTo(vmKey string) ([]v1alpha1.ApplyToPeer, error) {
	obj, exist, err := c.vmLister.GetByKey(vmKey)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, fmt.Errorf("vm %s not found", vmKey)
	}

	applyToPeers := make([]v1alpha1.ApplyToPeer, 0, len(obj.(*schema.VM).VMNics))
	for _, vnic := range obj.(*schema.VM).VMNics {
		vnicID := vnic.GetID()
		applyToPeers = append(applyToPeers, v1alpha1.ApplyToPeer{
			Endpoint: &vnicID,
		})
	}
	return applyToPeers, nil
}

func (c *Controller) parseNetworkPolicyRules(ingressRules, egressRules []schema.NetworkPolicyRule) (ingress, egress []v1alpha1.Rule, err error) {
	ingress = make([]v1alpha1.Rule, 0, len(ingressRules))
	egress = make([]v1alpha1.Rule, 0, len(egressRules))

	for item, rule := range ingressRules {
		peers, ports, err := c.parseNetworkPolicyRule(&ingressRules[item])
		if err != nil {
			return nil, nil, err
		}
		if len(peers) == 0 && rule.Type != schema.NetworkPolicyRuleTypeAll {
			// when no peer is specified, the rule is dropped
			// because of empty peer means match all in everoute security policy
			continue
		}
		ingress = append(ingress, v1alpha1.Rule{
			Name:  fmt.Sprintf("ingress%d", item),
			Ports: ports,
			From:  peers,
		})
	}

	for item, rule := range egressRules {
		peers, ports, err := c.parseNetworkPolicyRule(&egressRules[item])
		if err != nil {
			return nil, nil, err
		}
		if len(peers) == 0 && rule.Type != schema.NetworkPolicyRuleTypeAll {
			// when no peer is specified, the rule is dropped
			// because of empty peer means match all in everoute security policy
			continue
		}
		egress = append(egress, v1alpha1.Rule{
			Name:  fmt.Sprintf("egress%d", item),
			Ports: ports,
			To:    peers,
		})
	}

	return ingress, egress, nil
}

// parseNetworkPolicyRule parse NetworkPolicyRule to []v1alpha1.SecurityPolicyPeer and []v1alpha1.SecurityPolicyPort
func (c *Controller) parseNetworkPolicyRule(rule *schema.NetworkPolicyRule) ([]v1alpha1.SecurityPolicyPeer, []v1alpha1.SecurityPolicyPort, error) {
	var policyPeers []v1alpha1.SecurityPolicyPeer
	var policyPorts = make([]v1alpha1.SecurityPolicyPort, 0, len(rule.Ports))

	for _, port := range rule.Ports {
		policyPort, err := parseNetworkPolicyRulePort(port)
		if err != nil {
			return nil, nil, err
		}
		policyPorts = append(policyPorts, *policyPort)
	}
	for _, svc := range rule.Services {
		svcObj, exists, err := c.serviceLister.GetByKey(svc.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("can't find policy related service %s", svc.ID)
		}
		if !exists {
			klog.Errorf("policy related service %s doesn't exists", svc.ID)
			continue
		}
		svc := svcObj.(*schema.NetworkPolicyRuleService)
		svcPorts, err := parseNetworkPolicyService(svc)
		if err != nil {
			return nil, nil, fmt.Errorf("parse service %s failed: %s", svc.ID, err)
		}
		policyPorts = append(policyPorts, svcPorts...)
	}

	disableSymmetric := false
	if rule.OnlyApplyToExternalTraffic {
		disableSymmetric = true
	}

	switch rule.Type {
	case schema.NetworkPolicyRuleTypeAll:
		// empty PolicyPeers match all
	case schema.NetworkPolicyRuleTypeIPBlock:
		if rule.IPBlock == nil {
			return nil, nil, fmt.Errorf("receive rule.Type %s but empty IPBlock", schema.NetworkPolicyRuleTypeIPBlock)
		}
		ipBlocks, err := parseIPBlock(*rule.IPBlock, rule.ExceptIPBlock)
		if err != nil {
			return nil, nil, fmt.Errorf("parse IPBlock %s with except %v: %s", *rule.IPBlock, rule.ExceptIPBlock, err)
		}
		for _, ipBlock := range ipBlocks {
			policyPeers = append(policyPeers, v1alpha1.SecurityPolicyPeer{IPBlock: ipBlock, DisableSymmetric: disableSymmetric})
		}
	case schema.NetworkPolicyRuleTypeSelector:
		endpointSelector, err := c.parseSelectors(rule.Selector)
		if err != nil {
			return nil, nil, err
		}
		policyPeers = append(policyPeers, v1alpha1.SecurityPolicyPeer{
			EndpointSelector:  endpointSelector,
			DisableSymmetric:  disableSymmetric,
			NamespaceSelector: c.genNamespaceSelector(false),
		})
	case schema.NetworkPolicyRuleTypeSecurityGroup:
		if rule.SecurityGroup == nil {
			return nil, nil, fmt.Errorf("receive rule.Type %s but empty SecurityGroup", schema.NetworkPolicyRuleTypeSecurityGroup)
		}
		peers, isPod, err := c.parseSecurityGroup(rule.SecurityGroup)
		if err != nil {
			return nil, nil, err
		}
		policyPeers = append(policyPeers, c.appliedPeersAsPolicyPeers(peers, disableSymmetric, isPod)...)
	}

	return policyPeers, policyPorts, nil
}

func (c *Controller) parseSelectors(selectors []schema.ObjectReference) (*labels.Selector, error) {
	if len(selectors) == 0 {
		return &labels.Selector{MatchNothing: true}, nil
	}

	var matchLabels = make(map[string]string)
	var extendMatchLabels = make(map[string][]string)

	for _, labelRef := range selectors {
		obj, exist, err := c.labelLister.GetByKey(labelRef.ID)
		if err != nil || !exist {
			return nil, fmt.Errorf("label %s not found", labelRef.ID)
		}
		label := obj.(*schema.Label)
		extendMatchLabels[label.Key] = append(extendMatchLabels[label.Key], label.Value)
	}

	// For backward compatibility, we set valid labels in selector.matchLabels,
	// and for other labels, we set them in selector.extendMatchLabels.
	for key, valueSet := range extendMatchLabels {
		if len(valueSet) != 1 {
			continue
		}
		isValid := endpoint.ValidKubernetesLabel(&schema.Label{Key: key, Value: valueSet[0]})
		if isValid {
			matchLabels[key] = valueSet[0]
			delete(extendMatchLabels, key)
		}
	}

	labelSelector := labels.Selector{
		LabelSelector:     metav1.LabelSelector{MatchLabels: matchLabels},
		ExtendMatchLabels: extendMatchLabels,
	}
	return &labelSelector, nil
}

func (c *Controller) parseVMSecurityGroup(securityGroup *schema.SecurityGroup) ([]v1alpha1.ApplyToPeer, error) {
	var appliedPeers []v1alpha1.ApplyToPeer

	for _, vm := range securityGroup.VMs {
		peers, err := c.vmAsAppliedTo(vm.ID)
		if err != nil {
			return nil, err
		}
		appliedPeers = append(appliedPeers, peers...)
	}

	for _, labelGroup := range securityGroup.LabelGroups {
		endpointSelector, err := c.parseSelectors(labelGroup.Labels)
		if err != nil {
			return nil, err
		}
		appliedPeers = append(appliedPeers, v1alpha1.ApplyToPeer{
			EndpointSelector: endpointSelector,
		})
	}

	return appliedPeers, nil
}

func (c *Controller) parseIPSecurityGroup(securityGroup *schema.SecurityGroup) ([]v1alpha1.ApplyToPeer, error) {
	var appliedPeers []v1alpha1.ApplyToPeer

	ipBlocks, err := parseIPBlock(securityGroup.IPs, []string{securityGroup.ExcludeIPs})
	if err != nil {
		return appliedPeers, err
	}

	for _, item := range ipBlocks {
		appliedPeers = append(appliedPeers, v1alpha1.ApplyToPeer{
			IPBlock: item,
		})
	}

	return appliedPeers, nil
}

func (c *Controller) parsePodSecurityGroup(securityGroup *schema.SecurityGroup) ([]v1alpha1.ApplyToPeer, error) {
	var appliedPeers []v1alpha1.ApplyToPeer
	for _, podLabelGroup := range securityGroup.PodLabelGroups {
		matchLabels := make(map[string]string, 2)
		matchLabels[msconst.SKSLabelKeyClusterName] = utils.GetValidLabelString(podLabelGroup.KSC.Name)
		matchLabels[msconst.SKSLabelKeyClusterNamespace] = utils.GetValidLabelString(podLabelGroup.KSC.Namespace)
		for _, podLabel := range podLabelGroup.PodLabels {
			matchLabels[podLabel.Key] = podLabel.Value
		}
		selector := &labels.Selector{
			LabelSelector: metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
		}
		if len(podLabelGroup.Namespaces) > 0 {
			matchExpre := metav1.LabelSelectorRequirement{
				Key:      msconst.EICLabelKeyObjectNamespace,
				Operator: metav1.LabelSelectorOpIn,
			}
			for _, ns := range podLabelGroup.Namespaces {
				matchExpre.Values = append(matchExpre.Values, utils.GetValidLabelString(ns))
			}
			selector.LabelSelector.MatchExpressions = []metav1.LabelSelectorRequirement{matchExpre}
		}
		appliedPeers = append(appliedPeers, v1alpha1.ApplyToPeer{
			EndpointSelector: selector,
		})
	}
	return appliedPeers, nil
}

func (c *Controller) parseSecurityGroup(securityGroupRef *schema.ObjectReference) ([]v1alpha1.ApplyToPeer, bool, error) {
	obj, exist, err := c.securityGroupLister.GetByKey(securityGroupRef.ID)
	if err != nil || !exist {
		return nil, false, fmt.Errorf("security group %s not found", securityGroupRef.ID)
	}
	securityGroup := obj.(*schema.SecurityGroup)
	memberType := schema.VMGroupType
	if securityGroup.MemberType != nil {
		memberType = *securityGroup.MemberType
	}
	switch memberType {
	case schema.VMGroupType:
		vmPeers, err := c.parseVMSecurityGroup(securityGroup)
		return vmPeers, false, err
	case schema.PodGroupType:
		podPeers, err := c.parsePodSecurityGroup(securityGroup)
		return podPeers, true, err
	case schema.IPGroupType:
		ipPeers, err := c.parseIPSecurityGroup(securityGroup)
		return ipPeers, false, err
	default:
		return nil, false, fmt.Errorf("unknow securityGroup memberType")
	}
}

func (c *Controller) genNamespaceSelector(isPod bool) *metav1.LabelSelector {
	peerNs := c.namespace
	if isPod {
		peerNs = c.podNamespace
	}
	namespaceSelector := make(map[string]string, 1)
	namespaceSelector[K8sNsNameLabel] = peerNs
	return &metav1.LabelSelector{
		MatchLabels: namespaceSelector,
	}
}

func (c *Controller) appliedPeersAsPolicyPeers(appliedPeers []v1alpha1.ApplyToPeer, disableSymmetric bool, isPod bool) []v1alpha1.SecurityPolicyPeer {
	policyPeers := make([]v1alpha1.SecurityPolicyPeer, 0, len(appliedPeers))

	nsSelector := c.genNamespaceSelector(isPod)
	for _, appliedPeer := range appliedPeers {
		var namespacedEndpoint *v1alpha1.NamespacedName
		if appliedPeer.Endpoint != nil {
			namespacedEndpoint = &v1alpha1.NamespacedName{
				Name:      *appliedPeer.Endpoint,
				Namespace: c.namespace,
			}
		}

		peer := v1alpha1.SecurityPolicyPeer{
			Endpoint:         namespacedEndpoint,
			EndpointSelector: appliedPeer.EndpointSelector,
			IPBlock:          appliedPeer.IPBlock,
			DisableSymmetric: disableSymmetric,
		}
		if appliedPeer.EndpointSelector != nil && !appliedPeer.EndpointSelector.MatchNothing {
			peer.NamespaceSelector = nsSelector
		}
		policyPeers = append(policyPeers, peer)
	}

	return policyPeers
}

func (c *Controller) getSystemEndpointsPolicyKey() string {
	return c.namespace + "/" + SystemEndpointsPolicyName
}

func (c *Controller) getControllerPolicyKey() string {
	return c.namespace + "/" + ControllerPolicyName
}

func (c *Controller) getGlobalWhitelistPolicyKey() string {
	return c.namespace + "/" + GlobalWhitelistPolicyName
}

func parseIPBlock(ipBlock string, exceptsStr []string) ([]*networkingv1.IPBlock, error) {
	var block []*networkingv1.IPBlock
	var exceptAll []string
	var excepts []string

	for _, exceptItem := range exceptsStr {
		excepts = append(excepts, strings.Split(strings.Trim(exceptItem, ","), ",")...)
	}

	for _, item := range excepts {
		cidr, err := formatIPBlock(item)
		if err != nil {
			return nil, err
		}
		exceptAll = append(exceptAll, cidr...)
	}

	ipBlock = strings.Trim(ipBlock, ",")
	ipBlockList := strings.Split(ipBlock, ",")
	for _, item := range ipBlockList {
		cidrs, err := formatIPBlock(item)
		if err != nil {
			return nil, err
		}

	cidrLoop:
		for _, cidr := range cidrs {
			_, cidrNet, _ := net.ParseCIDR(cidr)
			var exceptValid []string
			for _, exceptItem := range exceptAll {
				if !utils.IsSameIPFamily(cidr, exceptItem) {
					continue
				}

				_, exceptItemCidr, _ := net.ParseCIDR(exceptItem)
				if cidrNet.Contains(exceptItemCidr.IP) ||
					cidrNet.Contains(ipaddr.NewPrefix(exceptItemCidr).Last()) ||
					exceptItemCidr.Contains(cidrNet.IP) ||
					exceptItemCidr.Contains(ipaddr.NewPrefix(cidrNet).Last()) {
					if cidr == exceptItem {
						continue cidrLoop
					}
					exceptValid = append(exceptValid, exceptItem)
				}
			}
			block = append(block, &networkingv1.IPBlock{
				CIDR:   cidr,
				Except: exceptValid,
			})
		}
	}

	return block, nil
}

func formatIPBlock(ipBlock string) ([]string, error) {
	ipBlock = strings.TrimSpace(ipBlock)

	if ipBlock == "" {
		return []string{}, nil
	}

	// for ip block
	_, _, err := net.ParseCIDR(ipBlock)
	if err == nil {
		return []string{ipBlock}, nil
	}

	// for ip range
	ipRange := strings.Split(ipBlock, "-")
	if len(ipRange) == 2 {
		ipStartStr := strings.TrimSpace(ipRange[0])
		ipEndStr := strings.TrimSpace(ipRange[1])

		if !utils.IsIPv4Pair(ipStartStr, ipEndStr) &&
			!utils.IsIPv6Pair(ipStartStr, ipEndStr) {
			return []string{}, fmt.Errorf("different ip family in ip range %s", ipRange)
		}

		ipStart := net.ParseIP(ipStartStr)
		ipEnd := net.ParseIP(ipEndStr)

		if ipStart == nil || ipEnd == nil {
			return []string{}, fmt.Errorf("invalid ip range %s", ipRange)
		}

		ipPrefix := ipaddr.Summarize(ipStart, ipEnd)
		var ret []string
		for _, pf := range ipPrefix {
			ret = append(ret, pf.String())
		}
		return ret, nil
	}

	// for single ip
	ip := net.ParseIP(ipBlock)
	if ip.Equal(net.IPv4zero) {
		return []string{"0.0.0.0/0"}, nil
	}
	if ip.Equal(net.IPv6zero) {
		return []string{"::/0"}, nil
	}
	if utils.IsIPv4(ipBlock) {
		return []string{fmt.Sprintf("%s/%d", ipBlock, 32)}, nil
	}
	if utils.IsIPv6(ipBlock) {
		return []string{fmt.Sprintf("%s/%d", ipBlock, 128)}, nil
	}

	return []string{""}, fmt.Errorf("neither %s is cidr nor ipv4 nor ipv6", ipBlock)
}

func parseEnforcementMode(mode schema.PolicyMode) v1alpha1.PolicyMode {
	switch mode {
	case schema.PolicyModeWork:
		return v1alpha1.WorkMode
	case schema.PolicyModeMonitor:
		return v1alpha1.MonitorMode
	default:
		// the default work mode is defined in the SecurityPolicy CRD
		return ""
	}
}

func parseNetworkPolicyRulePort(port schema.NetworkPolicyRulePort) (*v1alpha1.SecurityPolicyPort, error) {
	switch port.Protocol {
	case schema.NetworkPolicyRulePortProtocolIcmp, schema.NetworkPolicyRulePortProtocolIPIP:
		return &v1alpha1.SecurityPolicyPort{Protocol: v1alpha1.Protocol(port.Protocol)}, nil
	case schema.NetworkPolicyRulePortProtocolALG:
		switch port.AlgProtocol {
		case schema.NetworkPolicyRulePortAlgProtocolFTP:
			return &v1alpha1.SecurityPolicyPort{Protocol: v1alpha1.ProtocolTCP, PortRange: FTPPortRange}, nil
		case schema.NetworkPolicyRulePortAlgProtocolTFTP:
			return &v1alpha1.SecurityPolicyPort{Protocol: v1alpha1.ProtocolUDP, PortRange: TFTPPortRange}, nil
		default:
			return nil, fmt.Errorf("only support FTP and TFTP for alg protocol, but the alg protocol is %s, port: %+v", port.AlgProtocol, port)
		}
	default:
		portRange := ""
		if port.Port != nil {
			portRange = strings.ReplaceAll(*port.Port, " ", "")
			portRange = strings.Trim(portRange, ",")
		}
		return &v1alpha1.SecurityPolicyPort{Protocol: v1alpha1.Protocol(port.Protocol), PortRange: portRange}, nil
	}
}

func parseNetworkPolicyService(svc *schema.NetworkPolicyRuleService) ([]v1alpha1.SecurityPolicyPort, error) {
	ports := []v1alpha1.SecurityPolicyPort{}
	for i := range svc.Members {
		port, err := parseNetworkPolicyRulePort(svc.Members[i])
		if err != nil {
			return ports, nil
		}
		ports = append(ports, *port)
	}

	return ports, nil
}

func serviceMembersToSets(svc *schema.NetworkPolicyRuleService) sets.Set[string] {
	set := sets.New[string]()
	for i := range svc.Members {
		portStr := ""
		if svc.Members[i].Port != nil {
			portStr = *svc.Members[i].Port
		}
		set.Insert(string(svc.Members[i].Protocol) + portStr)
	}

	return set
}

func getGlobalWhitelistPolicyEnforceMode(enable bool) v1alpha1.PolicyMode {
	if enable {
		return v1alpha1.WorkMode
	}
	return v1alpha1.MonitorMode
}

func NewLoggingOptionsFrom(obj schema.Object, vmLister informer.Lister) *v1alpha1.Logging {
	switch t := obj.(type) {
	case *schema.EverouteCluster:
		return newLoggingOptions(t.EnableLogging, t.ID, "", msconst.LoggingTagPolicyTypeGlobalPolicy)
	case *schema.SecurityPolicy:
		pt := lo.If(t.IsBlocklist, msconst.LoggingTagPolicyTypeSecurityPolicyDeny).
			Else(msconst.LoggingTagPolicyTypeSecurityPolicyAllow)
		return newLoggingOptions(t.EnableLogging, t.ID, t.Name, pt)
	case *schema.IsolationPolicy:
		var name string
		if vmLister != nil {
			if vm, _, _ := vmLister.GetByKey(t.VM.ID); vm != nil {
				name = vm.(*schema.VM).Name
			}
		}
		return newLoggingOptions(t.EnableLogging, t.ID, name, msconst.LoggingTagPolicyTypeQuarantinePolicy)
	default:
		return newLoggingOptions(false, "", "", "")
	}
}

func newLoggingOptions(enabled bool, policyID, policyName, policyType string) *v1alpha1.Logging {
	return &v1alpha1.Logging{
		Enabled: enabled,
		Tags: map[string]string{
			msconst.LoggingTagPolicyID:   policyID,
			msconst.LoggingTagPolicyName: policyName,
			msconst.LoggingTagPolicyType: policyType,
		},
	}
}
