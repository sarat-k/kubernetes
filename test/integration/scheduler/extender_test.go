// +build integration,!no-etcd

/*
Copyright 2015 The Kubernetes Authors.

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

package scheduler

// This file tests scheduler extender.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	v1core "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/typed/core/v1"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/plugin/pkg/scheduler"
	_ "k8s.io/kubernetes/plugin/pkg/scheduler/algorithmprovider"
	schedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api"
	"k8s.io/kubernetes/plugin/pkg/scheduler/factory"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/integration/framework"
)

const (
	filter     = "filter"
	prioritize = "prioritize"
)

type fitPredicate func(pod *v1.Pod, node *v1.Node) (bool, error)
type priorityFunc func(pod *v1.Pod, nodes *v1.NodeList) (*schedulerapi.HostPriorityList, error)

type priorityConfig struct {
	function priorityFunc
	weight   int
}

type Extender struct {
	name             string
	predicates       []fitPredicate
	prioritizers     []priorityConfig
	nodeCacheCapable bool
}

var podAnnotations = schedulerapi.Annotations{
	"testann.io/pod-ann-foo": "foo",
	"testann.io/pod-ann-bar": "bar",
}

func (e *Extender) serveHTTP(t *testing.T, w http.ResponseWriter, req *http.Request) {
	var args schedulerapi.ExtenderArgs

	decoder := json.NewDecoder(req.Body)
	defer req.Body.Close()

	if err := decoder.Decode(&args); err != nil {
		http.Error(w, "Decode error", http.StatusBadRequest)
		return
	}

	encoder := json.NewEncoder(w)

	if strings.Contains(req.URL.Path, filter) {
		resp := &schedulerapi.ExtenderFilterResult{}
		resp, err := e.Filter(&args)
		if err != nil {
			resp.Error = err.Error()
		}

		if err := encoder.Encode(resp); err != nil {
			t.Fatalf("Failed to encode %+v", resp)
		}
	} else if strings.Contains(req.URL.Path, prioritize) {
		// Prioritize errors are ignored. Default k8s priorities or another extender's
		// priorities may be applied.
		priorities, _ := e.Prioritize(&args)

		if err := encoder.Encode(priorities); err != nil {
			t.Fatalf("Failed to encode %+v", priorities)
		}
	} else {
		http.Error(w, "Unknown method", http.StatusNotFound)
	}
}

func (e *Extender) filterUsingNodeCache(args *schedulerapi.ExtenderArgs) (*schedulerapi.ExtenderFilterResult, error) {
	nodeSlice := make([]string, len(*args.NodeNames))
	failedNodesMap := schedulerapi.FailedNodesMap{}

	for _, nodeName := range *args.NodeNames {
		fits := true
		for _, predicate := range e.predicates {
			fit, err := predicate(&args.pod,
				&v1.Node{ObjectMeta: api.ObjectMeta{Name: nodeName}})
			if err != nil {
				return &schedulerapi.ExtenderFilterResult{
					Nodes:          nil,
					NodeNames:      nil,
					PodAnnotations: &podAnnotations,
					FailedNodes:    schedulerapi.FailedNodesMap{},
					Error:          err.Error(),
				}, err
			}
			if !fit {
				fits = false
				break
			}
		}
		if fits {
			nodeSlice = append(nodeSlice, nodeName)
		} else {
			failedNodesMap[nodeName] = fmt.Sprintf("extender failed: %s", e.name)
		}
	}

	return &schedulerapi.ExtenderFilterResult{
		Nodes:          nil,
		NodeNames:      &nodeSlice,
		PodAnnotations: nil,
		FailedNodes:    failedNodesMap,
	}, nil
}

func (e *Extender) Filter(args *schedulerapi.ExtenderArgs) (*schedulerapi.ExtenderFilterResult, error) {
	filtered := []v1.Node{}
	failedNodesMap := schedulerapi.FailedNodesMap{}

	if e.nodeCacheCapable {
		return e.filterUsingNodeCache(args)
	} else {
		for _, node := range args.Nodes.Items {
			fits := true
			for _, predicate := range e.predicates {
				fit, err := predicate(&args.Pod, &node)
				if err != nil {
					return &schedulerapi.ExtenderFilterResult{
						Nodes:          &v1.NodeList{},
						NodeNames:      nil,
						PodAnnotations: nil,
						FailedNodes:    schedulerapi.FailedNodesMap{},
						Error:          err.Error(),
					}, err
				}
				if !fit {
					fits = false
					break
				}
			}
			if fits {
				filtered = append(filtered, node)
			} else {
				failedNodesMap[node.Name] = fmt.Sprintf("extender failed: %s", e.name)
			}
		}
		return &schedulerapi.ExtenderFilterResult{
			Nodes:          &v1.NodeList{Items: filtered},
			NodeNames:      nil,
			PodAnnotations: nil,
			FailedNodes:    failedNodesMap,
		}, nil
	}
}

func (e *Extender) Prioritize(args *schedulerapi.ExtenderArgs) (schedulerapi.HostPriorityList, error) {
	result := schedulerapi.HostPriorityList{}
	combinedScores := map[string]int{}
	for _, prioritizer := range e.prioritizers {
		weight := prioritizer.weight
		if weight == 0 {
			continue
		}
		priorityFunc := prioritizer.function
		prioritizedList, err := priorityFunc(&args.Pod, args.Nodes)
		if err != nil {
			return &schedulerapi.HostPriorityList{}, err
		}
		for _, hostEntry := range *prioritizedList {
			combinedScores[hostEntry.Host] += hostEntry.Score * weight
		}
	}
	for host, score := range combinedScores {
		result = append(result, schedulerapi.HostPriority{Host: host, Score: score})
	}
	return &result, nil
}

func machine_1_2_3_Predicate(pod *v1.Pod, node *v1.Node) (bool, error) {
	if node.Name == "machine1" || node.Name == "machine2" || node.Name == "machine3" {
		return true, nil
	}
	return false, nil
}

func machine_2_3_5_Predicate(pod *v1.Pod, node *v1.Node) (bool, error) {
	if node.Name == "machine2" || node.Name == "machine3" || node.Name == "machine5" {
		return true, nil
	}
	return false, nil
}

func machine_2_Prioritizer(pod *v1.Pod, nodes *v1.NodeList) (*schedulerapi.HostPriorityList, error) {
	result := schedulerapi.HostPriorityList{}
	for _, node := range nodes.Items {
		score := 1
		if node.Name == "machine2" {
			score = 10
		}
		result = append(result, schedulerapi.HostPriority{node.Name, score})
	}
	return &result, nil
}

func machine_3_Prioritizer(pod *v1.Pod, nodes *v1.NodeList) (*schedulerapi.HostPriorityList, error) {
	result := schedulerapi.HostPriorityList{}
	for _, node := range nodes.Items {
		score := 1
		if node.Name == "machine3" {
			score = 10
		}
		result = append(result, schedulerapi.HostPriority{node.Name, score})
	}
	return &result, nil
}

func TestSchedulerExtender(t *testing.T) {
	_, s := framework.RunAMaster(nil)
	defer s.Close()

	ns := framework.CreateTestingNamespace("scheduler-extender", s, t)
	defer framework.DeleteTestingNamespace(ns, s, t)

	clientSet := clientset.NewForConfigOrDie(&restclient.Config{Host: s.URL, ContentConfig: restclient.ContentConfig{GroupVersion: &api.Registry.GroupOrDie(v1.GroupName).GroupVersion}})

	extender1 := &Extender{
		name:         "extender1",
		predicates:   []fitPredicate{machine_1_2_3_Predicate},
		prioritizers: []priorityConfig{{machine_2_Prioritizer, 1}},
	}
	es1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		extender1.serveHTTP(t, w, req)
	}))
	defer es1.Close()

	extender2 := &Extender{
		name:         "extender2",
		predicates:   []fitPredicate{machine_2_3_5_Predicate},
		prioritizers: []priorityConfig{{machine_3_Prioritizer, 1}},
	}
	es2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		extender2.serveHTTP(t, w, req)
	}))
	defer es2.Close()

	extender3 := &Extender{
		name:             "extender3",
		predicates:       []fitPredicate{machine_1_2_3_Predicate},
		prioritizers:     []priorityConfig{{machine_2_Prioritizer, 5}},
		nodeCacheCapable: true,
	}
	es3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		extender3.serveHTTP(t, w, req)
	}))
	defer es3.Close()

	policy := schedulerapi.Policy{
		ExtenderConfigs: []schedulerapi.ExtenderConfig{
			{
				URLPrefix:      es1.URL,
				FilterVerb:     filter,
				PrioritizeVerb: prioritize,
				Weight:         3,
				EnableHttps:    false,
			},
			{
				URLPrefix:      es2.URL,
				FilterVerb:     filter,
				PrioritizeVerb: prioritize,
				Weight:         4,
				EnableHttps:    false,
			},
			{
				URLPrefix:        es3.URL,
				FilterVerb:       filter,
				PrioritizeVerb:   prioritize,
				Weight:           5,
				EnableHttps:      false,
				NodeCacheCapable: true,
			},
		},
	}
	policy.APIVersion = api.Registry.GroupOrDie(v1.GroupName).GroupVersion.String()

	schedulerConfigFactory := factory.NewConfigFactory(clientSet, v1.DefaultSchedulerName, v1.DefaultHardPodAffinitySymmetricWeight, v1.DefaultFailureDomains)
	schedulerConfig, err := schedulerConfigFactory.CreateFromConfig(policy)
	if err != nil {
		t.Fatalf("Couldn't create scheduler config: %v", err)
	}
	eventBroadcaster := record.NewBroadcaster()
	schedulerConfig.Recorder = eventBroadcaster.NewRecorder(v1.EventSource{Component: v1.DefaultSchedulerName})
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: clientSet.Core().Events("")})
	scheduler.New(schedulerConfig).Run()

	defer close(schedulerConfig.StopEverything)

	DoTestPodScheduling(ns, t, clientSet)
}

func DoTestPodScheduling(ns *v1.Namespace, t *testing.T, cs clientset.Interface) {
	// NOTE: This test cannot run in parallel, because it is creating and deleting
	// non-namespaced objects (Nodes).
	defer cs.Core().Nodes().DeleteCollection(nil, metav1.ListOptions{})

	goodCondition := v1.NodeCondition{
		Type:              v1.NodeReady,
		Status:            v1.ConditionTrue,
		Reason:            fmt.Sprintf("schedulable condition"),
		LastHeartbeatTime: metav1.Time{time.Now()},
	}
	node := &v1.Node{
		Spec: v1.NodeSpec{Unschedulable: false},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
			},
			Conditions: []v1.NodeCondition{goodCondition},
		},
	}

	for ii := 0; ii < 5; ii++ {
		node.Name = fmt.Sprintf("machine%d", ii+1)
		if _, err := cs.Core().Nodes().Create(node); err != nil {
			t.Fatalf("Failed to create nodes: %v", err)
		}
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "extender-test-pod"},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "container", Image: e2e.GetPauseImageName(cs)}},
		},
	}

	myPod, err := cs.Core().Pods(ns.Name).Create(pod)
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	err = wait.Poll(time.Second, wait.ForeverTestTimeout, podScheduled(cs, myPod.Namespace, myPod.Name))
	if err != nil {
		t.Fatalf("Failed to schedule pod: %v", err)
	}

	if myPod, err := cs.Core().Pods(ns.Name).Get(myPod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Failed to get pod: %v", err)
	} else if myPod.Spec.NodeName != "machine2" {
		t.Fatalf("Failed to schedule using extender, expected machine3, got %v", myPod.Spec.NodeName)
	}
	for k := range podAnnotations {
		if myPod.Annotations[k] != podAnnotations[k] {
			t.Fatalf("Pod annotations not matching, expected %s:%s, found %s:%s", k, podAnnotations[k], k, myPod.Annotations[k])
		}
	}
	t.Logf("Scheduled pod using extenders")
}
