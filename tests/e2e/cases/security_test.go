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

package cases

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"

	securityv1alpha1 "github.com/everoute/everoute/pkg/apis/security/v1alpha1"
	"github.com/everoute/everoute/pkg/constants"
	"github.com/everoute/everoute/pkg/labels"
	"github.com/everoute/everoute/tests/e2e/framework"
	"github.com/everoute/everoute/tests/e2e/framework/config"
	"github.com/everoute/everoute/tests/e2e/framework/ipam"
	"github.com/everoute/everoute/tests/e2e/framework/matcher"
	"github.com/everoute/everoute/tests/e2e/framework/model"
)

var _ = Describe("SecurityPolicy", func() {
	AfterEach(func() {
		Expect(e2eEnv.ResetResource(ctx)).Should(Succeed())
	})

	// This case test policy with tcp and icmp can works. We setup three groups of vms (nginx/webserver/database), create
	// and verify policy allow connection: all sources to nginx, nginx to webservers, webserver to databases, and connect
	// between databases.
	//
	//        |---------|          |----------- |          |---------- |
	//  --->  |  nginx  |  <---->  | webservers |  <---->  | databases |
	//        | --------|          |----------- |          |---------- |
	//
	Context("environment with endpoints provide public http service [Feature:TCP] [Feature:ICMP]", func() {
		var nginx, server01, server02, db01, db02, client *model.Endpoint
		var nginxSelector, serverSelector, dbSelector *labels.Selector
		var nginxPort, serverPort, dbPort int

		BeforeEach(func() {
			nginxPort, serverPort, dbPort = 443, 443, 3306

			nginx = &model.Endpoint{Name: "nginx", TCPPort: nginxPort, Labels: map[string][]string{"component": {"nginx"}}}
			server01 = &model.Endpoint{Name: "server01", TCPPort: serverPort, Labels: map[string][]string{"component": {"webserver"}}}
			server02 = &model.Endpoint{Name: "server02", TCPPort: serverPort, Labels: map[string][]string{"component": {"webserver"}}}
			db01 = &model.Endpoint{Name: "db01", TCPPort: dbPort, Labels: map[string][]string{"component": {"database"}}}
			db02 = &model.Endpoint{Name: "db02", TCPPort: dbPort, Labels: map[string][]string{"component": {"database"}}}
			client = &model.Endpoint{Name: "client"}

			nginxSelector = newSelector(map[string][]string{"component": {"nginx"}})
			serverSelector = newSelector(map[string][]string{"component": {"webserver"}})
			dbSelector = newSelector(map[string][]string{"component": {"database"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, nginx, server01, server02, db01, db02, client)).Should(Succeed())
		})

		When("limits tcp packets between components", func() {
			var nginxPolicy, serverPolicy, dbPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				nginxPolicy = newPolicy("nginx-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector)
				addIngressRule(nginxPolicy, "TCP", nginxPort) // allow all connection with nginx port
				addEngressRule(nginxPolicy, "TCP", serverPort, serverSelector)

				serverPolicy = newPolicy("server-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, serverSelector)
				addIngressRule(serverPolicy, "TCP", serverPort, nginxSelector)
				addEngressRule(serverPolicy, "TCP", dbPort, dbSelector)

				dbPolicy = newPolicy("db-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, dbSelector)
				addIngressRule(dbPolicy, "TCP", dbPort, dbSelector, serverSelector)
				addEngressRule(dbPolicy, "TCP", dbPort, dbSelector)

				Expect(e2eEnv.SetupObjects(ctx, nginxPolicy, serverPolicy, dbPolicy)).Should(Succeed())
				setupPolicyCopy(nginxPolicy, serverPolicy, dbPolicy)
			})

			It("should allow normal packets and limits illegal packets", func() {
				assertFlowMatches(&SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
					Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
				})

				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "TCP", false)

				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{nginx}, "TCP", true)
				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01, server02}, "TCP", true)
				assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{db01, db02}, "TCP", true)
			})

			When("add endpoint into the database group", func() {
				var db03 *model.Endpoint

				BeforeEach(func() {
					db03 = &model.Endpoint{Name: "db03", TCPPort: 3306, Labels: map[string][]string{"component": {"database"}}}
					Expect(e2eEnv.EndpointManager().SetupMany(ctx, db03)).Should(Succeed())
				})

				It("should allow normal packets and limits illegal packets for new member", func() {
					assertFlowMatches(&SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
						Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
					})

					// NOTE always success in this case, even if failed to add updated flow
					assertReachable([]*model.Endpoint{nginx, client}, []*model.Endpoint{db03}, "TCP", false)
					assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{db03}, "TCP", true)
				})
			})

			When("update endpoint ip addr in the nginx group", func() {
				BeforeEach(func() {
					Expect(e2eEnv.EndpointManager().RenewIPMany(ctx, nginx)).Should(Succeed())
				})

				It("should allow normal packets and limits illegal packets for update member", func() {
					assertFlowMatches(&SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
						Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
					})

					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{db01, db02}, "TCP", false)
					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01, server02}, "TCP", true)
				})
			})

			When("remove endpoint from the webserver group", func() {
				BeforeEach(func() {
					server02.Labels = map[string][]string{}
					Expect(e2eEnv.EndpointManager().UpdateMany(ctx, server02)).Should(Succeed())
				})

				It("should limits illegal packets for remove member", func() {
					assertFlowMatches(&SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
						Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
					})

					assertReachable([]*model.Endpoint{server02}, []*model.Endpoint{server01, db01, db02}, "TCP", false)
				})
			})

			When("Migrate endpoint from one node to another node", func() {
				BeforeEach(func() {
					if len(e2eEnv.NodeManager().ListAgent()) <= 1 {
						Skip("Require at least two agent")
					}
					Expect(e2eEnv.EndpointManager().MigrateMany(ctx, server01)).Should(Succeed())
				})

				It("Should limit connections between webserver group and other groups", func() {
					assertFlowMatches(&SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
						Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
					})

					assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, db01, db02}, "TCP", false)

					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", true)
					assertReachable([]*model.Endpoint{server01}, []*model.Endpoint{db01, db02}, "TCP", true)
				})
			})
		})

		When("create monitor mode security policies", func() {
			var nginxPolicy, serverPolicy, dbPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				nginxPolicy = newPolicy("nginx-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector)
				nginxPolicy.Spec.SecurityPolicyEnforcementMode = securityv1alpha1.MonitorMode
				addIngressRule(nginxPolicy, "TCP", nginxPort) // allow all connection with nginx port
				addEngressRule(nginxPolicy, "TCP", serverPort, serverSelector)

				serverPolicy = newPolicy("server-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, serverSelector)
				serverPolicy.Spec.SecurityPolicyEnforcementMode = securityv1alpha1.MonitorMode
				addIngressRule(serverPolicy, "TCP", serverPort, nginxSelector)
				addEngressRule(serverPolicy, "TCP", dbPort, dbSelector)

				dbPolicy = newPolicy("db-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, dbSelector)
				dbPolicy.Spec.SecurityPolicyEnforcementMode = securityv1alpha1.MonitorMode
				addIngressRule(dbPolicy, "TCP", dbPort, dbSelector, serverSelector)
				addEngressRule(dbPolicy, "TCP", dbPort, dbSelector)

				Expect(e2eEnv.SetupObjects(ctx, nginxPolicy, serverPolicy, dbPolicy)).Should(Succeed())
				setupPolicyCopy(nginxPolicy, serverPolicy, dbPolicy)
			})

			It("should allow all packets", func() {
				assertFlowMatches(&SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{nginxPolicy, serverPolicy, dbPolicy},
					Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
				})

				assertReachable([]*model.Endpoint{nginx},
					[]*model.Endpoint{server01, server02, db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{server01},
					[]*model.Endpoint{nginx, db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{db01},
					[]*model.Endpoint{nginx, server01, server02}, "TCP", true)

			})

		})

		When("limits icmp packets between components", func() {
			var icmpAllowPolicy, icmpDropPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				icmpDropPolicy = newPolicy("icmp-drop-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, serverSelector, dbSelector)
				addIngressRule(icmpDropPolicy, "TCP", 0) // allow all tcp packets

				icmpAllowPolicy = newPolicy("icmp-allow-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector)
				addIngressRule(icmpAllowPolicy, "ICMP", 0) // allow all icmp packets

				Expect(e2eEnv.SetupObjects(ctx, icmpAllowPolicy, icmpDropPolicy)).Should(Succeed())
			})

			It("should allow normal packets and limits illegal packets", func() {
				assertFlowMatches(&SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{icmpAllowPolicy, icmpDropPolicy},
					Endpoints: []*model.Endpoint{nginx, server01, server02, db01, db02, client},
				})

				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "ICMP", false)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{nginx}, "ICMP", true)
			})
		})

		Context("security policy with symmetric mode [Feature:SymmetricMode]", func() {
			var policy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				if e2eEnv.EndpointManager().Name() == "tower" {
					Skip("tower e2e has no disableSymmetric feature, skip it")
				}

				nginxDropPolicy := newPolicy("nginx-drop", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector)
				Expect(e2eEnv.SetupObjects(ctx, nginxDropPolicy)).Should(Succeed())

				policy = newPolicy("test-symmetric", constants.Tier2, securityv1alpha1.DefaultRuleDrop, serverSelector)
				addIngressRule(policy, "TCP", serverPort, nginxSelector)
			})

			When("create security policy enable SymmetricMode", func() {
				BeforeEach(func() {
					policy.Spec.SymmetricMode = true
					Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
				})

				It("should allow tcp packets because there are symmetric rules", func() {
					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", true)
				})

				When("set peer DisableSymmetric enable", func() {
					BeforeEach(func() {
						policy.Spec.IngressRules[0].From[0].DisableSymmetric = true
						Expect(e2eEnv.UpdateObjects(ctx, policy)).Should(Succeed())
					})

					It("shoulid limit tcp packets because there are no symmetric rules", func() {
						assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", false)
					})
				})

				When("set peer DisableSymmetric disable", func() {
					BeforeEach(func() {
						policy.Spec.IngressRules[0].From[0].DisableSymmetric = false
						Expect(e2eEnv.UpdateObjects(ctx, policy)).Should(Succeed())
					})

					It("shoulid limit tcp packets because there are symmetric rules", func() {
						assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", true)
					})

					When("disable SymmetricMode", func() {
						BeforeEach(func() {
							policy.Spec.SymmetricMode = false
							Expect(e2eEnv.UpdateObjects(ctx, policy)).Should(Succeed())
						})

						It("should limit tcp packets because there are no symmetric rules", func() {
							assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", false)
						})
					})
				})
			})

			When("create security policy disable SymmetricMode", func() {
				BeforeEach(func() {
					policy.Spec.SymmetricMode = false
					Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
				})

				It("should limit tcp packets because there are no symmetric rules", func() {
					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", false)
				})

				When("set peer DisableSymmetric enable", func() {
					BeforeEach(func() {
						policy.Spec.IngressRules[0].From[0].DisableSymmetric = true
						Expect(e2eEnv.UpdateObjects(ctx, policy)).Should(Succeed())
					})

					It("should limit tcp packets because there are no symmetric rules, disableSymmetric can't change policy symmetric mode when SymmetricMode=false", func() {
						assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", false)
					})
				})

				When("set peer DisableSymmetric disable", func() {
					BeforeEach(func() {
						policy.Spec.IngressRules[0].From[0].DisableSymmetric = false
						Expect(e2eEnv.UpdateObjects(ctx, policy)).Should(Succeed())
					})

					It("should limit tcp packets because there are no symmetric rules, disableSymmetric can't change policy symmetric mode when SymmetricMode=false", func() {
						assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01}, "TCP", false)
					})
				})
			})

		})

		Context("blocklist securitypolicy, [Feature:blocklist]", func() {
			var nginxPolicy, serverPolicy, dbPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				nginxPolicy = newPolicy("nginx-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector)
				addIngressRule(nginxPolicy, "TCP", nginxPort) // allow all connection with nginx port
				addEngressRule(nginxPolicy, "TCP", serverPort, serverSelector)

				serverPolicy = newPolicy("server-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, serverSelector)
				addIngressRule(serverPolicy, "TCP", serverPort, nginxSelector)
				addEngressRule(serverPolicy, "TCP", dbPort, dbSelector)

				dbPolicy = newPolicy("db-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, dbSelector)
				addIngressRule(dbPolicy, "TCP", dbPort, dbSelector, serverSelector)
				addEngressRule(dbPolicy, "TCP", dbPort, dbSelector)

				Expect(e2eEnv.SetupObjects(ctx, nginxPolicy, serverPolicy, dbPolicy)).Should(Succeed())

				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{nginx}, "TCP", true)

				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01, server02}, "TCP", true)

				assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{nginx}, "TCP", false)
			})
			It("setup blocklist", func() {
				By("blocklist is workmode")
				var dbPolicyBlocklist *securityv1alpha1.SecurityPolicy
				dbPolicyBlocklist = newPolicy("db-policy-blocklist", constants.Tier2, securityv1alpha1.DefaultRuleNone, dbSelector)
				addIngressRule(dbPolicyBlocklist, "TCP", dbPort, serverSelector)
				setBlocklistPolicy(dbPolicyBlocklist)
				Expect(e2eEnv.SetupObjects(ctx, dbPolicyBlocklist)).Should(Succeed())

				By("check reachable for blocklist")
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{nginx}, "TCP", true)

				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01, server02}, "TCP", true)

				assertReachable([]*model.Endpoint{db01, db02}, []*model.Endpoint{db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{nginx}, "TCP", false)
				assertReachable([]*model.Endpoint{server01, server02}, []*model.Endpoint{db01, db02}, "TCP", false)

				By("update blocklist to monitor mode")
				dbPolicyBlocklist.Spec.SecurityPolicyEnforcementMode = securityv1alpha1.MonitorMode
				Expect(e2eEnv.UpdateObjects(ctx, dbPolicyBlocklist)).Should(Succeed())

				By("check reachable for blocklist with monitor mode")
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{server01, server02, db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{client}, []*model.Endpoint{nginx}, "TCP", true)

				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{db01, db02}, "TCP", false)
				assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server01, server02}, "TCP", true)

				assertReachable([]*model.Endpoint{db01, db02}, []*model.Endpoint{db01, db02}, "TCP", true)
				assertReachable([]*model.Endpoint{server01, server02, db01, db02}, []*model.Endpoint{nginx}, "TCP", false)
				assertReachable([]*model.Endpoint{server01, server02}, []*model.Endpoint{db01, db02}, "TCP", true)
			})
		})
	})
	Context("environment with endpoints provide public ftp service [Feature:FTP]", func() {
		var ftpServer, client *model.Endpoint
		var ftpSelector *labels.Selector
		var tcpPort = 9090

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "tower" {
				Skip("tower e2e has no alg feature, skip it")
			}

			ftpServer = &model.Endpoint{Name: "ftp-server", TCPPort: tcpPort, Proto: "FTP", Labels: map[string][]string{"component": {"ftpserver"}}}
			ftpSelector = newSelector(map[string][]string{"component": {"ftpserver"}})
			client = &model.Endpoint{Name: "client"}

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, ftpServer, client)).Should(Succeed())
			assertReachable([]*model.Endpoint{client}, []*model.Endpoint{ftpServer}, "TCP", true)
			assertReachable([]*model.Endpoint{client}, []*model.Endpoint{ftpServer}, "FTP", true)
		})
		It("create security policy only allow ftp", func() {
			policy := newPolicy("allow-ftp", constants.Tier2, securityv1alpha1.DefaultRuleDrop, ftpSelector)
			addIngressRule(policy, "TCP", 21)
			Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
			assertReachable([]*model.Endpoint{client}, []*model.Endpoint{ftpServer}, "TCP", false)
			assertReachable([]*model.Endpoint{client}, []*model.Endpoint{ftpServer}, "FTP", true)
		})
	})

	Context("endpoint isolation [Feature:ISOLATION]", func() {
		var ep01, ep02, ep03, ep04 *model.Endpoint
		var forensicGroup *labels.Selector
		var forensicGroup2 *labels.Selector
		var tcpPort int

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "tower" || e2eEnv.EndpointManager().Name() == "pod" {
				Skip("isolation vm from tower need tower support")
			}
			tcpPort = 443

			ep01 = &model.Endpoint{Name: "ep01", TCPPort: tcpPort}
			ep02 = &model.Endpoint{Name: "ep02", TCPPort: tcpPort}
			ep03 = &model.Endpoint{Name: "ep03", TCPPort: tcpPort}
			ep04 = &model.Endpoint{Name: "ep04", TCPPort: tcpPort}

			forensicGroup = newSelector(map[string][]string{"component": {"forensic"}})
			forensicGroup2 = newSelector(map[string][]string{"component": {"forensic2"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, ep01, ep02, ep03, ep04)).Should(Succeed())
		})

		When("Isolate endpoint", func() {
			var isolationPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				isolationPolicy = newPolicy("isolation-policy", constants.Tier0, securityv1alpha1.DefaultRuleDrop, ep01.Name)

				Expect(e2eEnv.SetupObjects(ctx, isolationPolicy)).Should(Succeed())
			})

			It("Isolated endpoint should not allow to communicate with all of endpoint", func() {
				securityModel := &SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{isolationPolicy},
					Endpoints: []*model.Endpoint{ep01, ep02, ep03, ep04},
				}

				By("verify all agents has correct flows")
				assertFlowMatches(securityModel)

				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(true)
				expectedTruthTable.SetAllFrom(ep01.Name, false)
				expectedTruthTable.SetAllTo(ep01.Name, false)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})
		})

		When("Forensic endpoint", func() {
			var forensicPolicy1 *securityv1alpha1.SecurityPolicy
			var forensicPolicy2 *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				forensicPolicy1 = newPolicy("forensic-policy-ingress", constants.Tier1, securityv1alpha1.DefaultRuleDrop, ep01.Name)
				forensicPolicy1.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
				addIngressRule(forensicPolicy1, "TCP", tcpPort, forensicGroup)

				forensicPolicy2 = newPolicy("forensic-policy-egress", constants.Tier0, securityv1alpha1.DefaultRuleDrop, ep01.Name)
				forensicPolicy2.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

				// set ep02 as forensic endpoint
				ep02.Labels = map[string][]string{"component": {"forensic"}}

				Expect(e2eEnv.EndpointManager().UpdateMany(ctx, ep02)).Should(Succeed())
				Expect(e2eEnv.SetupObjects(ctx, forensicPolicy1, forensicPolicy2)).Should(Succeed())
			})

			It("Isolated endpoint should not allow to communicate with all of endpoint except forensic defined allowed endpoint", func() {
				securityModel := &SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{forensicPolicy1, forensicPolicy2},
					Endpoints: []*model.Endpoint{ep01, ep02, ep03, ep04},
				}

				By("verify all agents has correct flows")
				assertFlowMatches(securityModel)

				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(true)
				expectedTruthTable.SetAllFrom(ep01.Name, false)
				expectedTruthTable.SetAllTo(ep01.Name, false)
				expectedTruthTable.Set(ep02.Name, ep01.Name, true)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})

			When("forensic update endpoint status after setup policy", func() {
				BeforeEach(func() {
					Expect(e2eEnv.EndpointManager().RenewIPMany(ctx, ep01)).Should(Succeed())
				})

				It("Isolated endpoint should not allow to communicate with all of endpoint except forensic defined allowed endpoint", func() {
					securityModel := &SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{forensicPolicy1, forensicPolicy2},
						Endpoints: []*model.Endpoint{ep01, ep02, ep03, ep04},
					}

					By("verify all agents has correct flows")
					assertFlowMatches(securityModel)

					By("verify reachable between endpoints")
					expectedTruthTable := securityModel.NewEmptyTruthTable(true)
					expectedTruthTable.SetAllFrom(ep01.Name, false)
					expectedTruthTable.SetAllTo(ep01.Name, false)
					expectedTruthTable.Set(ep02.Name, ep01.Name, true)
					assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
				})
			})
		})

		When("Isolate & Forensic endpoint", func() {
			var forensicPolicy1 *securityv1alpha1.SecurityPolicy
			var forensicPolicy2 *securityv1alpha1.SecurityPolicy
			var isolationPolicy *securityv1alpha1.SecurityPolicy
			BeforeEach(func() {
				forensicPolicy1 = newPolicy("forensic-policy-ingress", constants.Tier1, securityv1alpha1.DefaultRuleDrop, ep02.Name)
				forensicPolicy1.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
				addIngressRule(forensicPolicy1, "TCP", tcpPort, forensicGroup)

				forensicPolicy2 = newPolicy("forensic-policy-egress", constants.Tier0, securityv1alpha1.DefaultRuleDrop, ep02.Name)
				forensicPolicy2.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

				// set ep02 as forensic endpoint
				ep01.Labels = map[string][]string{"component": {"forensic"}}

				Expect(e2eEnv.EndpointManager().UpdateMany(ctx, ep01)).Should(Succeed())
				Expect(e2eEnv.SetupObjects(ctx, forensicPolicy1, forensicPolicy2)).Should(Succeed())

				isolationPolicy = newPolicy("isolation-policy", constants.Tier0, securityv1alpha1.DefaultRuleDrop, ep01.Name)
				Expect(e2eEnv.SetupObjects(ctx, isolationPolicy)).Should(Succeed())
			})
			It("isolation policy should have higher priority than forensic policy", func() {
				securityModel := &SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{forensicPolicy1, forensicPolicy2, isolationPolicy},
					Endpoints: []*model.Endpoint{ep01, ep02, ep03, ep04},
				}

				By("verify all agents has correct flows")
				assertFlowMatches(securityModel)

				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(true)
				expectedTruthTable.SetAllFrom(ep01.Name, false)
				expectedTruthTable.SetAllTo(ep01.Name, false)
				expectedTruthTable.SetAllFrom(ep02.Name, false)
				expectedTruthTable.SetAllTo(ep02.Name, false)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)

			})
			When("Isolate & Forensic endpoint", func() {
				var forensicPolicy3 *securityv1alpha1.SecurityPolicy
				var forensicPolicy4 *securityv1alpha1.SecurityPolicy
				BeforeEach(func() {
					forensicPolicy3 = newPolicy("forensic-policy2-ingress", constants.Tier1, securityv1alpha1.DefaultRuleDrop, ep03.Name)
					forensicPolicy3.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
					addIngressRule(forensicPolicy3, "TCP", tcpPort, forensicGroup2)

					forensicPolicy4 = newPolicy("forensic-policy2-egress", constants.Tier0, securityv1alpha1.DefaultRuleDrop, ep03.Name)
					forensicPolicy4.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

					// set ep02 as forensic endpoint
					ep02.Labels = map[string][]string{"component": {"forensic2"}}

					Expect(e2eEnv.EndpointManager().UpdateMany(ctx, ep02)).Should(Succeed())
					Expect(e2eEnv.SetupObjects(ctx, forensicPolicy3, forensicPolicy4)).Should(Succeed())

				})
				It("forensic policy should have effect while cross assigned", func() {
					securityModel := &SecurityModel{
						Policies:  []*securityv1alpha1.SecurityPolicy{forensicPolicy1, forensicPolicy2, forensicPolicy3, forensicPolicy4},
						Endpoints: []*model.Endpoint{ep01, ep02, ep03, ep04},
					}

					By("verify all agents has correct flows")
					assertFlowMatches(securityModel)

					By("verify reachable between endpoints")
					expectedTruthTable := securityModel.NewEmptyTruthTable(true)
					expectedTruthTable.SetAllFrom(ep01.Name, false)
					expectedTruthTable.SetAllTo(ep01.Name, false)
					expectedTruthTable.SetAllFrom(ep02.Name, false)
					expectedTruthTable.SetAllTo(ep02.Name, false)
					expectedTruthTable.SetAllFrom(ep03.Name, false)
					expectedTruthTable.SetAllTo(ep03.Name, false)
					assertMatchReachTable("TCP", tcpPort, expectedTruthTable)

				})
			})
		})
	})

	// This case test policy with udp and ipblocks can works. We setup two peers ntp server and client in different cidr,
	// create and verify policy allow connect with ntp in its cidr.
	//
	//  |----------------|         |--------------- |    |---------------- |         |--------------- |
	//  | "10.0.0.0/28"  |  <--->  | ntp-production |    | ntp-development |  <--->  | "10.0.0.16/28" |
	//  | ---------------|         |--------------- |    |---------------- |         |--------------- |
	//
	Context("environment with endpoints provide internal udp service [Feature:UDP] [Feature:IPBlocks]", func() {
		var ntp01, ntp02, client01, client02 *model.Endpoint
		var ntpProductionSelector, ntpDevelopmentSelector *labels.Selector

		var ntpPort int
		var productionCidr, developmentCidr string

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "pod" {
				Skip("pod cannot assign ExpectSubnet")
			}

			ntpPort = 123
			productionCidr = "10.0.0.0/28"
			developmentCidr = "10.0.0.16/28"

			client01 = &model.Endpoint{Name: "ntp-client01", ExpectSubnet: productionCidr}
			client02 = &model.Endpoint{Name: "ntp-client02", ExpectSubnet: developmentCidr}
			ntp01 = &model.Endpoint{Name: "ntp01-server", ExpectSubnet: productionCidr, UDPPort: ntpPort, Labels: map[string][]string{"component": {"ntp"}, "env": {"production"}}}
			ntp02 = &model.Endpoint{Name: "ntp02-server", ExpectSubnet: developmentCidr, UDPPort: ntpPort, Labels: map[string][]string{"component": {"ntp"}, "env": {"development"}}}

			ntpProductionSelector = newSelector(map[string][]string{"component": {"ntp"}, "env": {"production"}})
			ntpDevelopmentSelector = newSelector(map[string][]string{"component": {"ntp"}, "env": {"development"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, ntp01, ntp02, client01, client02)).Should(Succeed())
		})

		When("limits udp packets by ipBlocks between server and client", func() {
			var ntpProductionPolicy, ntpDevelopmentPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				ntpProductionPolicy = newPolicy("ntp-production-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, ntpProductionSelector)
				addIngressRule(ntpProductionPolicy, "UDP", ntpPort, &networkingv1.IPBlock{CIDR: productionCidr})

				ntpDevelopmentPolicy = newPolicy("ntp-development-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, ntpDevelopmentSelector)
				addIngressRule(ntpDevelopmentPolicy, "UDP", ntpPort, &networkingv1.IPBlock{CIDR: developmentCidr})

				Expect(e2eEnv.SetupObjects(ctx, ntpProductionPolicy, ntpDevelopmentPolicy)).Should(Succeed())
			})

			It("should allow normal packets and limits illegal packets", func() {
				By("verify agent has correct open flows")
				assertFlowMatches(&SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{ntpProductionPolicy, ntpDevelopmentPolicy},
					Endpoints: []*model.Endpoint{ntp01, ntp02, client01, client02},
				})

				By("verify policy limits illegal packets")
				assertReachable([]*model.Endpoint{client01}, []*model.Endpoint{ntp02}, "UDP", false)
				assertReachable([]*model.Endpoint{client02}, []*model.Endpoint{ntp01}, "UDP", false)

				By("verify reachable between servers")
				assertReachable([]*model.Endpoint{ntp01}, []*model.Endpoint{ntp02}, "UDP", false)
				assertReachable([]*model.Endpoint{ntp02}, []*model.Endpoint{ntp01}, "UDP", false)

				By("verify reachable between server and client")
				assertReachable([]*model.Endpoint{client01}, []*model.Endpoint{ntp01}, "UDP", true)
				assertReachable([]*model.Endpoint{client02}, []*model.Endpoint{ntp02}, "UDP", true)
			})
		})
	})

	Context("Complicated securityPolicy definition that contains semanticly conflict policyrules", func() {
		var group1Endpoint1, group2Endpoint01, group3Endpoint01 *model.Endpoint
		var group1, group2, group3 *labels.Selector
		var epTCPPort int

		BeforeEach(func() {
			epTCPPort = 80

			group1Endpoint1 = &model.Endpoint{
				Name:    "group1-ep01",
				TCPPort: epTCPPort,
				Labels:  map[string][]string{"group": {"group1"}},
			}
			group2Endpoint01 = &model.Endpoint{
				Name:    "group2-ep01",
				TCPPort: epTCPPort,
				Labels:  map[string][]string{"group": {"group2"}},
			}
			group3Endpoint01 = &model.Endpoint{
				Name:    "group3-ep01",
				TCPPort: epTCPPort,
				Labels:  map[string][]string{"group": {"group3"}},
			}

			group1 = newSelector(map[string][]string{"group": {"group1"}})
			group2 = newSelector(map[string][]string{"group": {"group2"}})
			group3 = newSelector(map[string][]string{"group": {"group3"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, group1Endpoint1, group2Endpoint01, group3Endpoint01)).Should(Succeed())
		})

		When("Define securityPolicy without semanticly conflicts with any of securityPolicy already exists", func() {
			var securityPolicy1, securityPolicy2 *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				securityPolicy1 = newPolicy("group1-policy", constants.Tier0, securityv1alpha1.DefaultRuleDrop, group1)
				addIngressRule(securityPolicy1, "TCP", epTCPPort, group2)
				securityPolicy1.Spec.SymmetricMode = true

				Expect(e2eEnv.SetupObjects(ctx, securityPolicy1)).Should(Succeed())
			})

			AfterEach(func() {
				Expect(e2eEnv.CleanObjects(ctx, securityPolicy1)).Should(Succeed())
			})

			It("should allow group2 to communicate with group1", func() {
				assertReachable([]*model.Endpoint{group2Endpoint01}, []*model.Endpoint{group1Endpoint1}, "TCP", true)
			})

			When("Define a securityPolicy which semanticly conflict with existing securityPolicy", func() {
				BeforeEach(func() {
					securityPolicy2 = newPolicy("group2-policy", constants.Tier0, securityv1alpha1.DefaultRuleDrop, group2)
					addEngressRule(securityPolicy2, "TCP", epTCPPort, group3)

					Expect(e2eEnv.SetupObjects(ctx, securityPolicy2)).Should(Succeed())
				})

				AfterEach(func() {
					Expect(e2eEnv.CleanObjects(ctx, securityPolicy2)).Should(Succeed())
				})

				It("should deny group2 to communicate with group1", func() {
					assertReachable([]*model.Endpoint{group2Endpoint01}, []*model.Endpoint{group1Endpoint1}, "TCP", true)
				})
			})
		})
	})

	// This case would setup endpoints in random vlan, and check reachable between them.
	Context("environment with endpoints from specify vlan [Feature:VLAN]", func() {
		var groupA, groupB *labels.Selector
		var endpointA, endpointB, endpointC *model.Endpoint
		var tcpPort, vlanID int

		BeforeEach(func() {
			tcpPort = rand.IntnRange(1000, 5000)
			vlanID = rand.IntnRange(0, 4095)

			endpointA = &model.Endpoint{Name: "ep.a", VID: vlanID, TCPPort: tcpPort, Labels: map[string][]string{"group": {"gx"}}}
			endpointB = &model.Endpoint{Name: "ep.b", VID: vlanID, TCPPort: tcpPort, Labels: map[string][]string{"group": {"gy"}}}
			endpointC = &model.Endpoint{Name: "ep.c", VID: vlanID, TCPPort: tcpPort, Labels: map[string][]string{"group": {"gz"}}}

			groupA = newSelector(map[string][]string{"group": {"gx"}})
			groupB = newSelector(map[string][]string{"group": {"gy"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, endpointA, endpointB, endpointC)).Should(Succeed())
		})

		When("limits tcp packets between components", func() {
			var groupPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				// allow traffic from groupA to groupB
				groupPolicy = newPolicy("group-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, groupA)
				addEngressRule(groupPolicy, "TCP", tcpPort, groupB)
				Expect(e2eEnv.SetupObjects(ctx, groupPolicy)).Should(Succeed())
			})

			It("should allow normal packets and limits illegal packets", func() {
				securityModel := &SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{groupPolicy},
					Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
				}

				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(true)
				expectedTruthTable.SetAllFrom(endpointA.Name, false)
				expectedTruthTable.SetAllTo(endpointA.Name, false)
				expectedTruthTable.Set(endpointA.Name, endpointB.Name, true)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})
		})
	})

	Context("environment with endpoints has multiple labels with same key [Feature:ExtendLabels]", func() {
		var groupA, groupB *labels.Selector
		var endpointA, endpointB, endpointC *model.Endpoint
		var tcpPort int

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "pod" {
				Skip("pod do not have multiple labels with same key ")
			}

			tcpPort = rand.IntnRange(1000, 5000)

			endpointA = &model.Endpoint{Name: "ep.a", TCPPort: tcpPort, Labels: map[string][]string{"@中文标签": {"&单标签值"}}}
			endpointB = &model.Endpoint{Name: "ep.b", TCPPort: tcpPort, Labels: map[string][]string{"@中文标签": {"?@!#@@$$$"}}}
			endpointC = &model.Endpoint{Name: "ep.c", TCPPort: tcpPort, Labels: map[string][]string{"@中文标签": {"?@!#@@$$$", "=>多标签值"}}}

			groupA = newSelector(map[string][]string{"@中文标签": {"&单标签值"}})
			groupB = newSelector(map[string][]string{"@中文标签": {"?@!#@@$$$"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, endpointA, endpointB, endpointC)).Should(Succeed())
		})

		When("limits tcp packets between components", func() {
			var groupPolicy *securityv1alpha1.SecurityPolicy

			BeforeEach(func() {
				// allow traffic from groupA to groupB
				groupPolicy = newPolicy("group-policy", constants.Tier2, securityv1alpha1.DefaultRuleDrop, groupA)
				addEngressRule(groupPolicy, "TCP", tcpPort, groupB)
				Expect(e2eEnv.SetupObjects(ctx, groupPolicy)).Should(Succeed())
			})

			It("should allow normal packets and limits illegal packets", func() {
				securityModel := &SecurityModel{
					Policies:  []*securityv1alpha1.SecurityPolicy{groupPolicy},
					Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
				}

				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(true)
				expectedTruthTable.SetAllFrom(endpointA.Name, false)
				expectedTruthTable.SetAllTo(endpointA.Name, false)
				expectedTruthTable.Set(endpointA.Name, endpointB.Name, true)
				expectedTruthTable.Set(endpointA.Name, endpointC.Name, true)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})
		})
	})

	Context("endpoint with ipip tunnel [Feature:IPIP]", func() {
		var ipipEp1, ipipEp2 *model.Endpoint
		var ep1InternalIP, ep2InternalIP string
		var ipipSelector *labels.Selector
		var tcpPort = 7878

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "tower" {
				Skip("tower e2e has no ipip feature, skip it")
			}
			if e2eEnv.EndpointManager().Name() == "pod" {
				Skip("pod e2e has no ipip feature, skip it")
			}
			ipipEp1 = &model.Endpoint{Name: "ipip-1", TCPPort: tcpPort, Labels: map[string][]string{"component": {"ipip"}}}
			ipipEp2 = &model.Endpoint{Name: "ipip-2", Labels: map[string][]string{"component": {"client"}}}
			ipipSelector = newSelector(map[string][]string{"component": {"ipip"}})
			Expect(e2eEnv.EndpointManager().SetupMany(ctx, ipipEp1, ipipEp2)).Should(Succeed())

			ipPool, _ := ipam.NewPool(&config.IPAMConfig{IPRange: "15.19.0.1/24"})
			ep1InternalIP, _ = ipPool.Assign()
			ep2InternalIP, _ = ipPool.Assign()
			Expect(e2eEnv.EndpointManager().SetupIPIP(ctx, ipipEp1.Name, ipipEp2.Status.IPAddr, ipipEp1.Status.IPAddr, ep1InternalIP)).Should(Succeed())
			Expect(e2eEnv.EndpointManager().SetupIPIP(ctx, ipipEp2.Name, ipipEp1.Status.IPAddr, ipipEp2.Status.IPAddr, ep2InternalIP)).Should(Succeed())

			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "ICMP", true, ep1InternalIP)
			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "TCP", true)
		})

		It("limit IPIP packets", func() {
			policy := newPolicy("test-ipip", constants.Tier2, securityv1alpha1.DefaultRuleDrop, ipipSelector)
			addIngressRule(policy, "TCP", tcpPort)
			Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "ICMP", false, ep1InternalIP)
			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "TCP", true)
		})

		It("allow IPIP packets", func() {
			policy := newPolicy("test-ipip", constants.Tier2, securityv1alpha1.DefaultRuleDrop, ipipSelector)
			addIngressRule(policy, "IPIP", 0)
			Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "ICMP", true, ep1InternalIP)
			assertReachable([]*model.Endpoint{ipipEp2}, []*model.Endpoint{ipipEp1}, "TCP", false)
		})
	})

	Context("ecp networkPolicy [Feature:TierECP]", func() {
		var nginx, server, db *model.Endpoint
		var nginxSelector, serverSelector, dbSelector *labels.Selector
		var nginxPort, serverPort, dbPort = 443, 443, 3306

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "tower" {
				Skip("tower e2e has no TierECP feature, skip it")
			}
			if e2eEnv.EndpointManager().Name() == "pod" {
				Skip("pod e2e has no TierECP feature, skip it")
			}

			nginx = &model.Endpoint{Name: "nginx", TCPPort: nginxPort, Labels: map[string][]string{"component": {"nginx"}}}
			server = &model.Endpoint{Name: "server", TCPPort: serverPort, Labels: map[string][]string{"component": {"webserver"}}}
			db = &model.Endpoint{Name: "db", TCPPort: dbPort, Labels: map[string][]string{"component": {"database"}}}

			nginxSelector = newSelector(map[string][]string{"component": {"nginx"}})
			serverSelector = newSelector(map[string][]string{"component": {"webserver"}})
			dbSelector = newSelector(map[string][]string{"component": {"database"}})

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, nginx, server, db)).Should(Succeed())

			assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "ICMP", true)
			assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "TCP", true)
		})

		When("create security policy with tier2 deny traffic", func() {
			BeforeEach(func() {
				tier2Policy := newPolicy("tier2-deny", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector, serverSelector, dbSelector)
				Expect(e2eEnv.SetupObjects(ctx, tier2Policy)).Should(Succeed())
			})

			It("should limit all traffic", func() {
				assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "ICMP", false)
				assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "TCP", false)
			})

			When("create security policy with tier-ecp allow traffic", func() {
				BeforeEach(func() {
					tierECPServerPolicy := newPolicy("tier-ecp-server", constants.TierECP, securityv1alpha1.DefaultRuleNone, serverSelector)
					addIngressRule(tierECPServerPolicy, "TCP", serverPort, nginxSelector)
					addEngressRule(tierECPServerPolicy, "TCP", dbPort, dbSelector)

					tierECPDbPolicy := newPolicy("tier-ecp-db", constants.TierECP, securityv1alpha1.DefaultRuleNone, dbSelector)
					addIngressRule(tierECPDbPolicy, "TCP", dbPort, serverSelector)

					Expect(e2eEnv.SetupObjects(ctx, tierECPServerPolicy, tierECPDbPolicy)).Should(Succeed())
				})

				It("the tier-ecp policy can allow traffic which tier2 policy deny", func() {
					By("tier-ecp allow")
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db}, "TCP", true)
					By("tier2 deny")
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{nginx}, "TCP", false)
					assertReachable([]*model.Endpoint{nginx, db}, []*model.Endpoint{server, db, nginx}, "TCP", false)
					assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "ICMP", false)
				})
			})
		})

		When("create security policy with tier2 allow traffic", func() {
			BeforeEach(func() {
				tier2Policy := newPolicy("tier2-allow", constants.Tier2, securityv1alpha1.DefaultRuleDrop, nginxSelector, serverSelector, dbSelector)
				addIngressRule(tier2Policy, "TCP", nginxPort, nginxSelector, serverSelector, dbSelector)
				addIngressRule(tier2Policy, "TCP", dbPort, nginxSelector, serverSelector, dbSelector)
				addIngressRule(tier2Policy, "TCP", serverPort, nginxSelector, serverSelector, dbSelector)
				addIngressRule(tier2Policy, "ICMP", 0, nginxSelector, serverSelector, dbSelector)
				addEngressRule(tier2Policy, "TCP", nginxPort, nginxSelector, serverSelector, dbSelector)
				addEngressRule(tier2Policy, "TCP", dbPort, nginxSelector, serverSelector, dbSelector)
				addEngressRule(tier2Policy, "TCP", serverPort, nginxSelector, serverSelector, dbSelector)
				addEngressRule(tier2Policy, "ICMP", 0, nginxSelector, serverSelector, dbSelector)
				Expect(e2eEnv.SetupObjects(ctx, tier2Policy)).Should(Succeed())
			})

			It("should allow all traffic", func() {
				assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "ICMP", true)
				assertReachable([]*model.Endpoint{nginx, server, db}, []*model.Endpoint{nginx, server, db}, "TCP", true)
			})

			When("add security policy with tier-ecp", func() {
				BeforeEach(func() {
					tierECPServerPolicy := newPolicy("tier-ecp-server", constants.TierECP, securityv1alpha1.DefaultRuleDrop, serverSelector)
					addIngressRule(tierECPServerPolicy, "TCP", serverPort, nginxSelector)
					addEngressRule(tierECPServerPolicy, "TCP", dbPort, dbSelector)

					tierECPDbPolicy := newPolicy("tier-ecp-db", constants.TierECP, securityv1alpha1.DefaultRuleDrop, dbSelector)
					addIngressRule(tierECPDbPolicy, "TCP", dbPort, serverSelector)

					Expect(e2eEnv.SetupObjects(ctx, tierECPServerPolicy, tierECPDbPolicy)).Should(Succeed())
				})

				It("the tier-ecp policy can deny traffic which tier2 policy allow", func() {
					By("tier-ecp allow")
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db}, "TCP", true)
					By("tier-ecp deny")
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{nginx}, "TCP", false)
					assertReachable([]*model.Endpoint{db}, []*model.Endpoint{nginx, server}, "TCP", false)
					By("tier2 allow")
					assertReachable([]*model.Endpoint{nginx}, []*model.Endpoint{server}, "TCP", true)
				})
			})
		})

		When("create isolation policy", func() {
			BeforeEach(func() {
				isolationPolicy := newPolicy("iso-policy", constants.Tier0, securityv1alpha1.DefaultRuleDrop, serverSelector)
				Expect(e2eEnv.SetupObjects(ctx, isolationPolicy)).Should(Succeed())
			})

			It("should deny traffic for isolation policy", func() {
				By("isolation server")
				assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db, nginx}, "TCP", false)
				assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db, nginx}, "ICMP", false)
				assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{server}, "TCP", false)
				assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{server}, "ICMP", false)
				By("allow default")
				assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{db, nginx}, "TCP", true)
				assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{db, nginx}, "ICMP", true)
			})

			When("add security policy with tier-ecp allow isolation endpoints", func() {
				BeforeEach(func() {
					policy := newPolicy("allow-tier-ecp", constants.TierECP, securityv1alpha1.DefaultRuleNone, serverSelector)
					addIngressRule(policy, "TCP", serverPort, nginxSelector, dbSelector)
					addIngressRule(policy, "ICMP", 0, nginxSelector, dbSelector)
					addEngressRule(policy, "TCP", nginxPort, nginxSelector)
					addEngressRule(policy, "TCP", dbPort, dbSelector)
					addEngressRule(policy, "ICMP", 0, nginxSelector, dbSelector)
					Expect(e2eEnv.SetupObjects(ctx, policy)).Should(Succeed())
				})

				It("tier ecp can't allow traffic which has been isolation", func() {
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db, nginx}, "TCP", false)
					assertReachable([]*model.Endpoint{server}, []*model.Endpoint{db, nginx}, "ICMP", false)
					assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{server}, "TCP", false)
					assertReachable([]*model.Endpoint{db, nginx}, []*model.Endpoint{server}, "ICMP", false)
				})
			})
		})
	})
})

var _ = Describe("GlobalPolicy", func() {
	AfterEach(func() {
		Expect(e2eEnv.ResetResource(ctx)).Should(Succeed())
	})

	BeforeEach(func() {
		if e2eEnv.EndpointManager().Name() == "pod" {
			Skip("cni do not support global policy")
		}
	})

	Context("environment with global drop policy [Feature:GlobalPolicy]", func() {
		var endpointA, endpointB, endpointC *model.Endpoint
		var tcpPort int

		BeforeEach(func() {
			tcpPort = rand.IntnRange(1000, 5000)

			endpointA = &model.Endpoint{Name: "ep.a", TCPPort: tcpPort}
			endpointB = &model.Endpoint{Name: "ep.b", TCPPort: tcpPort}
			endpointC = &model.Endpoint{Name: "ep.c", TCPPort: tcpPort}

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, endpointA, endpointB, endpointC)).Should(Succeed())
		})

		It("should allow all traffics between endpoints", func() {
			securityModel := &SecurityModel{
				Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
			}
			By("verify reachable between endpoints")
			expectedTruthTable := securityModel.NewEmptyTruthTable(true)
			assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
		})

		It("should clean exist allow connection add global drop policy", func() {
			securityModel := &SecurityModel{
				Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
			}
			By("verify reachable between endpoints")
			expectedTruthTable := securityModel.NewEmptyTruthTable(true)
			assertMatchReachTable("TCP", tcpPort, expectedTruthTable)

			healthyChan := checkConnectionHealth(endpointA, endpointB)
			time.Sleep(2 * time.Second)

			Expect(e2eEnv.GlobalPolicyProvider().SetDefaultAction(ctx, securityv1alpha1.GlobalDefaultActionDrop)).Should(Succeed())

			Expect(<-healthyChan == UNHEALTHY).Should(BeTrue())
		})

		When("update global default action to drop", func() {
			BeforeEach(func() {
				// drop all traffics between endpoints
				Expect(e2eEnv.GlobalPolicyProvider().SetDefaultAction(ctx, securityv1alpha1.GlobalDefaultActionDrop)).Should(Succeed())
			})

			It("should limits all traffics between endpoints", func() {
				securityModel := &SecurityModel{
					Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
				}
				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(false)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})

			When("update global default action to allow", func() {
				BeforeEach(func() {
					By("wait for global drop policy add to datapath")
					time.Sleep(5 * time.Second)

					By("update global policy to default allow")
					Expect(e2eEnv.GlobalPolicyProvider().SetDefaultAction(ctx, securityv1alpha1.GlobalDefaultActionAllow)).Should(Succeed())
				})

				It("should allow all traffics between endpoints", func() {
					securityModel := &SecurityModel{
						Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
					}
					By("verify reachable between endpoints")
					expectedTruthTable := securityModel.NewEmptyTruthTable(true)
					assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
				})
			})
		})
	})

	Context("environment with global white list policy [Feature:GlobalWhitelistPolicy]", func() {
		var endpointA, endpointB, endpointC *model.Endpoint
		var tcpPort int
		var internalPolicyA, whitelistPolicy *securityv1alpha1.SecurityPolicy

		BeforeEach(func() {
			if e2eEnv.EndpointManager().Name() == "tower" {
				Skip("skip verify policy applied to endpoint directly")
			}

			tcpPort = rand.IntnRange(1000, 5000)

			endpointA = &model.Endpoint{Name: "ep.a", TCPPort: tcpPort}
			endpointB = &model.Endpoint{Name: "ep.b", TCPPort: tcpPort}
			endpointC = &model.Endpoint{Name: "ep.c", TCPPort: tcpPort}

			Expect(e2eEnv.EndpointManager().SetupMany(ctx, endpointA, endpointB, endpointC)).Should(Succeed())

			// drop all traffics between endpoints
			Expect(e2eEnv.GlobalPolicyProvider().SetDefaultAction(ctx, securityv1alpha1.GlobalDefaultActionDrop)).Should(Succeed())

			// add ingress all and egress all for endpoints A, set endpoint A as an outside vm
			internalPolicyA = newPolicy("internal-policy-a", constants.Tier2, securityv1alpha1.DefaultRuleNone)
			internalPolicyA.Spec.AppliedTo = []securityv1alpha1.ApplyToPeer{
				{
					Endpoint: &endpointA.Name,
				},
			}
			internalPolicyA.Spec.IngressRules = []securityv1alpha1.Rule{
				{Name: "ingress"},
			}
			internalPolicyA.Spec.EgressRules = []securityv1alpha1.Rule{
				{Name: "egress"},
			}
			Expect(e2eEnv.SetupObjects(ctx, internalPolicyA)).Should(Succeed())
		})
		When("add global whitelist ip with ingress", func() {
			BeforeEach(func() {
				whitelistPolicy = newPolicy("whitelist", constants.Tier2, securityv1alpha1.DefaultRuleNone)
				whitelistPolicy.Spec.IngressRules = []securityv1alpha1.Rule{
					{Name: "ingress", From: []securityv1alpha1.SecurityPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: strings.Split(endpointA.Status.IPAddr, "/")[0] + "/32"}}}},
				}
				Expect(e2eEnv.SetupObjects(ctx, whitelistPolicy)).Should(Succeed())
			})
			It("should allow traffics between endpointA to others", func() {
				securityModel := &SecurityModel{
					Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
				}
				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(false)
				expectedTruthTable.SetAllFrom(endpointA.Name, true)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})
		})

		When("add global whitelist ip with egress", func() {
			BeforeEach(func() {
				whitelistPolicy = newPolicy("whitelist", constants.Tier2, securityv1alpha1.DefaultRuleNone)
				whitelistPolicy.Spec.EgressRules = []securityv1alpha1.Rule{
					{Name: "egress", To: []securityv1alpha1.SecurityPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: strings.Split(endpointA.Status.IPAddr, "/")[0] + "/32"}}}},
				}
				Expect(e2eEnv.SetupObjects(ctx, whitelistPolicy)).Should(Succeed())
			})
			It("should allow traffics between endpointA to others", func() {
				securityModel := &SecurityModel{
					Endpoints: []*model.Endpoint{endpointA, endpointB, endpointC},
				}
				By("verify reachable between endpoints")
				expectedTruthTable := securityModel.NewEmptyTruthTable(false)
				expectedTruthTable.SetAllTo(endpointA.Name, true)
				assertMatchReachTable("TCP", tcpPort, expectedTruthTable)
			})
		})

	})
})

func newSelector(selector map[string][]string) *labels.Selector {
	return &labels.Selector{
		ExtendMatchLabels: selector,
	}
}

func newPolicy(name, tier string, defaultRule securityv1alpha1.DefaultRuleType, appliedPeers ...interface{}) *securityv1alpha1.SecurityPolicy {
	policy := &securityv1alpha1.SecurityPolicy{}
	policy.Name = name
	policy.Namespace = e2eEnv.Namespace()
	policy.Labels = map[string]string{framework.E2EPolicyLabelKey: framework.E2EPolicyLabelValue}
	policy.Spec.Tier = tier
	policy.Spec.DefaultRule = defaultRule
	policy.Spec.PolicyTypes = []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	}

	for _, appliedPeer := range appliedPeers {
		switch peer := appliedPeer.(type) {

		case *labels.Selector:
			policy.Spec.AppliedTo = append(policy.Spec.AppliedTo, securityv1alpha1.ApplyToPeer{
				EndpointSelector: peer,
			})

		case string:
			policy.Spec.AppliedTo = append(policy.Spec.AppliedTo, securityv1alpha1.ApplyToPeer{
				Endpoint: &peer,
			})

		default:
			panic(fmt.Sprintf("unsupport peer type %T", appliedPeer))
		}
	}

	return policy
}

func setupPolicyCopy(policyList ...*securityv1alpha1.SecurityPolicy) {
	for _, policy := range policyList {
		policyCopy := policy.DeepCopy()
		var policyNew securityv1alpha1.SecurityPolicy
		policyNew.Name = policyCopy.Name + "-copy"
		policyNew.Namespace = policyCopy.Namespace
		policyNew.Labels = policyCopy.Labels
		policyNew.Spec = policyCopy.Spec

		Expect(e2eEnv.SetupObjects(ctx, &policyNew)).Should(Succeed())
	}
}

func addIngressRule(policy *securityv1alpha1.SecurityPolicy, protocol string, port int, policyPeers ...interface{}) {
	ingressRule := &securityv1alpha1.Rule{
		Name: rand.String(20),
		Ports: []securityv1alpha1.SecurityPolicyPort{
			{
				Protocol:  securityv1alpha1.Protocol(protocol),
				PortRange: strconv.Itoa(port),
			},
		},
		From: getPolicyPeer(policyPeers...),
	}

	policy.Spec.IngressRules = append(policy.Spec.IngressRules, *ingressRule)
}

func setBlocklistPolicy(policy *securityv1alpha1.SecurityPolicy) {
	policy.Spec.Priority = 50
	policy.Spec.IsBlocklist = true
	policy.Spec.SymmetricMode = false
	policy.Spec.DefaultRule = securityv1alpha1.DefaultRuleNone
}

func addEngressRule(policy *securityv1alpha1.SecurityPolicy, protocol string, port int, policyPeers ...interface{}) {
	egressRule := &securityv1alpha1.Rule{
		Name: rand.String(20),
		Ports: []securityv1alpha1.SecurityPolicyPort{
			{
				Protocol:  securityv1alpha1.Protocol(protocol),
				PortRange: strconv.Itoa(port),
			},
		},
		To: getPolicyPeer(policyPeers...),
	}

	policy.Spec.EgressRules = append(policy.Spec.EgressRules, *egressRule)
}

func getPolicyPeer(policyPeers ...interface{}) []securityv1alpha1.SecurityPolicyPeer {
	var peerList []securityv1alpha1.SecurityPolicyPeer

	for _, policyPeer := range policyPeers {
		switch peer := policyPeer.(type) {

		case *labels.Selector:
			peerList = append(peerList, securityv1alpha1.SecurityPolicyPeer{
				EndpointSelector: peer,
			})

		case *types.NamespacedName:
			peerList = append(peerList, securityv1alpha1.SecurityPolicyPeer{
				Endpoint: &securityv1alpha1.NamespacedName{
					Name:      peer.Name,
					Namespace: peer.Namespace,
				},
			})

		case *networkingv1.IPBlock:
			peerList = append(peerList, securityv1alpha1.SecurityPolicyPeer{
				IPBlock: peer,
			})

		case *securityv1alpha1.SecurityPolicyPeer:
			peerList = append(peerList, *peer)

		default:
			panic(fmt.Sprintf("unsupport peer type %T", policyPeer))
		}
	}

	return peerList
}

func assertFlowMatches(securityModel *SecurityModel) {
	expectFlows := securityModel.ExpectedRelativeFlows()
	Expect(expectFlows).ShouldNot(BeEmpty())

	Eventually(func() map[string][]string {
		allFlows, err := e2eEnv.NodeManager().DumpFlowAll()
		Expect(err).Should(Succeed())
		return allFlows
	}, e2eEnv.Timeout(), e2eEnv.Interval()).Should(matcher.ContainsRelativeFlow(expectFlows))
}

func assertReachable(sources []*model.Endpoint, destinations []*model.Endpoint, protocol string, expectReach bool, extraArgs ...string) {
	Eventually(func() error {
		var errList []error

		for _, src := range sources {
			for _, dst := range destinations {
				if src.Name == dst.Name {
					continue
				}
				var port int
				if protocol == "TCP" {
					port = dst.TCPPort
				}
				if protocol == "UDP" {
					port = dst.UDPPort
				}
				reach, err := e2eEnv.EndpointManager().Reachable(ctx, src.Name, dst.Name, protocol, port, extraArgs...)
				Expect(err).Should(Succeed())

				if reach == expectReach {
					continue
				}
				errList = append(errList,
					fmt.Errorf("get reachable %t, want %t. src: %+v, dst: %+v, protocol: %s", reach, expectReach, src, dst, protocol),
				)
			}
		}
		return errors.NewAggregate(errList)
	}, e2eEnv.Timeout(), e2eEnv.Interval()).Should(Succeed())
}

func assertMatchReachTable(protocol string, port int, expectedTruthTable *model.TruthTable) {
	Eventually(func() *model.TruthTable {
		ctx, cancel := context.WithTimeout(ctx, e2eEnv.Timeout())
		defer cancel()

		tt, err := e2eEnv.EndpointManager().ReachTruthTable(ctx, protocol, port)
		Expect(err).Should(Succeed())
		return tt
	}, e2eEnv.Timeout(), e2eEnv.Interval()).Should(matcher.MatchTruthTable(expectedTruthTable, true))
}

type ConnHealth string

const (
	DISCONNECTED ConnHealth = "disconnected"
	UNHEALTHY    ConnHealth = "unhealthy"
	HEALTHY      ConnHealth = "healthy"
	UNKNOWN      ConnHealth = "unknown"
)

const CheckConnectionHealthTime int = 20 // 20s

func checkConnectionHealth(src, dst *model.Endpoint) <-chan ConnHealth {
	resultChan := make(chan ConnHealth)
	klog.Info("Check connection health from endpoint ", src.Name, " to endpoint ", dst.Name, ".")
	go func(src, dst *model.Endpoint, resultChan chan ConnHealth) {
		var command string = "ping"
		var args []string = []string{"-W", "1", "-c", strconv.Itoa(CheckConnectionHealthTime / 1), "-q", strings.Split(dst.Status.IPAddr, "/")[0]}
		rc, b, err := e2eEnv.EndpointManager().RunCommand(ctx, src.Name, command, args...)
		fullOut := string(b)
		if err != nil {
			klog.Error("Error check connection health endpoint {", src.Name, ",", dst.Name, "}, return code:", rc, ", error:", err)
			resultChan <- UNKNOWN
			return
		}
		// Expect 4 lines at least:
		// PING [dstIP] ([dstIP]): 56 data bytes
		//
		// --- [dstIP] ping statistics ---
		// [time] packets transmitted, [n1] packets received, [n2]% packet loss
		output := regexp.MustCompile("\r\n|\n|\r").Split(fullOut, -1)
		infoOutput := output[3] // expect: [n0] packets transmitted, [n1] packets received, [n2]% packet loss
		connInfo := regexp.MustCompile("[0-9]+").FindAllString(infoOutput, -1)
		klog.Info("Total,Received,Loss Rate,Time:", connInfo)
		lossRate, err := strconv.Atoi(connInfo[2])
		if err != nil {
			resultChan <- UNKNOWN
		} else if lossRate == 0 {
			resultChan <- HEALTHY
		} else if lossRate == 100 {
			resultChan <- DISCONNECTED
		} else {
			resultChan <- UNHEALTHY
		}
	}(src, dst, resultChan)
	return resultChan
}
