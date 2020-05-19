/*
Copyright 2017 The Kubernetes Authors.

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

package plank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/mohae/deepcopy"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/pjutil"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type fca struct {
	sync.Mutex
	c *config.Config
}

const (
	podPendingTimeout     = time.Hour
	podRunningTimeout     = time.Hour * 2
	podUnscheduledTimeout = time.Minute * 5
)

func newFakeConfigAgent(t *testing.T, maxConcurrency int) *fca {
	presubmits := []config.Presubmit{
		{
			JobBase: config.JobBase{
				Name: "test-bazel-build",
			},
		},
		{
			JobBase: config.JobBase{
				Name: "test-e2e",
			},
		},
		{
			AlwaysRun: true,
			JobBase: config.JobBase{
				Name: "test-bazel-test",
			},
		},
	}
	if err := config.SetPresubmitRegexes(presubmits); err != nil {
		t.Fatal(err)
	}
	presubmitMap := map[string][]config.Presubmit{
		"kubernetes/kubernetes": presubmits,
	}

	return &fca{
		c: &config.Config{
			ProwConfig: config.ProwConfig{
				ProwJobNamespace: "prowjobs",
				PodNamespace:     "pods",
				Plank: config.Plank{
					Controller: config.Controller{
						JobURLTemplate: template.Must(template.New("test").Parse("{{.ObjectMeta.Name}}/{{.Status.State}}")),
						MaxConcurrency: maxConcurrency,
						MaxGoroutines:  20,
					},
					PodPendingTimeout:     &metav1.Duration{Duration: podPendingTimeout},
					PodRunningTimeout:     &metav1.Duration{Duration: podRunningTimeout},
					PodUnscheduledTimeout: &metav1.Duration{Duration: podUnscheduledTimeout},
				},
			},
			JobConfig: config.JobConfig{
				PresubmitsStatic: presubmitMap,
			},
		},
	}
}

func (f *fca) Config() *config.Config {
	f.Lock()
	defer f.Unlock()
	return f.c
}

func TestTerminateDupes(t *testing.T) {
	now := time.Now()
	nowFn := func() *metav1.Time {
		reallyNow := metav1.NewTime(now)
		return &reallyNow
	}
	type testCase struct {
		Name string

		PJs  []prowapi.ProwJob
		PM   map[string]v1.Pod
		IsV2 bool

		TerminatedPJs  sets.String
		TerminatedPods sets.String
	}
	var testcases = []testCase{
		{
			Name: "terminate all duplicates",

			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j1",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j1",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "older", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j1",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-2 * time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "complete", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j1",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime:      metav1.NewTime(now.Add(-3 * time.Hour)),
						CompletionTime: nowFn(),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest_j2", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j2",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old_j2", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j2",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old_j3", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j3",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "new_j3", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type: prowapi.PresubmitJob,
						Job:  "j3",
						Refs: &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
			},

			TerminatedPJs: sets.NewString("old", "older", "old_j2", "old_j3"),
		},
		{
			Name: "should also terminate pods",

			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type:    prowapi.PresubmitJob,
						Job:     "j1",
						Refs:    &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
						PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Minute)),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "prowjobs"},
					Spec: prowapi.ProwJobSpec{
						Type:    prowapi.PresubmitJob,
						Job:     "j1",
						Refs:    &prowapi.Refs{Pulls: []prowapi.Pull{{}}},
						PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					},
					Status: prowapi.ProwJobStatus{
						StartTime: metav1.NewTime(now.Add(-time.Hour)),
					},
				},
			},
			PM: map[string]v1.Pod{
				"newest": {ObjectMeta: metav1.ObjectMeta{Name: "newest", Namespace: "pods"}},
				"old":    {ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "pods"}},
			},

			TerminatedPJs:  sets.NewString("old"),
			TerminatedPods: sets.NewString("old"),
		},
	}

	// Duplicate all tests for PlankV2
	for _, tc := range testcases {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		testcases = append(testcases, newTc)
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			var prowJobs []runtime.Object
			for i := range tc.PJs {
				pj := &tc.PJs[i]
				prowJobs = append(prowJobs, pj)
			}
			fakeProwJobClient := &patchTrackingFakeClient{
				Client: fakectrlruntimeclient.NewFakeClient(prowJobs...),
			}
			var pods []runtime.Object
			for name := range tc.PM {
				pod := tc.PM[name]
				pods = append(pods, &pod)
			}
			fakePodClient := &deleteTrackingFakeClient{
				Client: fakectrlruntimeclient.NewFakeClient(pods...),
			}
			fca := &fca{
				c: &config.Config{
					ProwConfig: config.ProwConfig{
						ProwJobNamespace: "prowjobs",
						PodNamespace:     "pods",
					},
				},
			}
			log := logrus.NewEntry(logrus.StandardLogger())

			if !tc.IsV2 {
				c := Controller{
					prowJobClient: fakeProwJobClient,
					buildClients:  map[string]ctrlruntimeclient.Client{prowapi.DefaultClusterAlias: fakePodClient},
					log:           log,
					config:        fca.Config,
					clock:         clock.RealClock{},
				}
				if err := c.terminateDupes(tc.PJs, tc.PM); err != nil {
					t.Fatalf("Error terminating dupes: %v", err)
				}

			} else {
				r := &reconciler{
					pjClient:     fakeProwJobClient,
					buildClients: map[string]ctrlruntimeclient.Client{prowapi.DefaultClusterAlias: fakePodClient},
					log:          log,
					config:       fca.Config,
					clock:        clock.RealClock{},
				}
				for _, pj := range tc.PJs {
					res, err := r.reconcile(&pj)
					if res != nil {
						err = utilerrors.NewAggregate([]error{err, fmt.Errorf("expected reconcile.Result to be nil, was %v", res)})
					}
					if err != nil {
						t.Fatalf("Error terminating dupes: %v", err)
					}
				}
			}

			observedCompletedProwJobs := fakeProwJobClient.patched
			if missing := tc.TerminatedPJs.Difference(observedCompletedProwJobs); missing.Len() > 0 {
				t.Errorf("did not delete expected prowJobs: %v", missing.List())
			}
			if extra := observedCompletedProwJobs.Difference(tc.TerminatedPJs); extra.Len() > 0 {
				t.Errorf("found unexpectedly deleted prowJobs: %v", extra.List())
			}

			observedTerminatedPods := fakePodClient.deleted
			if missing := tc.TerminatedPods.Difference(observedTerminatedPods); missing.Len() > 0 {
				t.Errorf("did not delete expected pods: %v", missing.List())
			}
			if extra := observedTerminatedPods.Difference(tc.TerminatedPods); extra.Len() > 0 {
				t.Errorf("found unexpectedly deleted pods: %v", extra.List())
			}
		})
	}
}

func handleTot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "42")
}

func TestSyncTriggeredJobs(t *testing.T) {
	fakeClock := clock.NewFakeClock(time.Now().Truncate(1 * time.Second))
	pendingTime := metav1.NewTime(fakeClock.Now())

	type testCase struct {
		Name string

		PJ             prowapi.ProwJob
		PendingJobs    map[string]int
		MaxConcurrency int
		Pods           map[string][]v1.Pod
		PodErr         error
		IsV2           bool

		ExpectedState       prowapi.ProwJobState
		ExpectedPodHasName  bool
		ExpectedNumPods     map[string]int
		ExpectedComplete    bool
		ExpectedURL         string
		ExpectedBuildID     string
		ExpectError         bool
		ExpectedPendingTime *metav1.Time
	}

	var testcases = []testCase{
		{
			Name: "start new pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "blabla",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods:                map[string][]v1.Pod{"default": {}},
			ExpectedState:       prowapi.PendingState,
			ExpectedPendingTime: &pendingTime,
			ExpectedPodHasName:  true,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedURL:         "blabla/pending",
		},
		{
			Name: "pod with a max concurrency of 1",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "blabla",
					Namespace:         "prowjobs",
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "same",
					Type:           prowapi.PeriodicJob,
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PendingJobs: map[string]int{
				"same": 1,
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "same-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:   prowapi.TriggeredState,
			ExpectedNumPods: map[string]int{"default": 1},
		},
		{
			Name: "trusted pod with a max concurrency of 1",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "blabla",
					Namespace:         "prowjobs",
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "same",
					Type:           prowapi.PeriodicJob,
					Cluster:        "trusted",
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PendingJobs: map[string]int{
				"same": 1,
			},
			Pods: map[string][]v1.Pod{
				"trusted": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "same-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:   prowapi.TriggeredState,
			ExpectedNumPods: map[string]int{"trusted": 1},
		},
		{
			Name: "trusted pod with a max concurrency of 1 (can start)",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "some",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:            "some",
					Type:           prowapi.PeriodicJob,
					Cluster:        "trusted",
					MaxConcurrency: 1,
					PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "other-42",
							Namespace: "pods",
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
				"trusted": {},
			},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1, "trusted": 1},
			ExpectedPodHasName:  true,
			ExpectedPendingTime: &pendingTime,
			ExpectedURL:         "some/pending",
		},
		{
			Name: "do not exceed global maxconcurrency",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "same",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			MaxConcurrency: 20,
			PendingJobs:    map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			ExpectedState:  prowapi.TriggeredState,
		},
		{
			Name: "global maxconcurrency allows new jobs when possible",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "same",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods:                map[string][]v1.Pod{"default": {}},
			MaxConcurrency:      21,
			PendingJobs:         map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedURL:         "beer/pending",
			ExpectedPendingTime: &pendingTime,
		},
		{
			Name: "unprocessable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusUnprocessableEntity,
				Reason: metav1.StatusReasonInvalid,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "forbidden prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusForbidden,
				Reason: metav1.StatusReasonForbidden,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "conflict error starting pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{"default": {}},
			PodErr: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusConflict,
				Reason: metav1.StatusReasonAlreadyExists,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
		},
		{
			Name: "unknown error starting pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "beer",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			PodErr:        errors.New("no way unknown jose"),
			ExpectedState: prowapi.TriggeredState,
			ExpectError:   true,
		},
		{
			Name: "running pod, failed prowjob update",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PeriodicJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.TriggeredState,
				},
			},
			Pods: map[string][]v1.Pod{
				"default": {
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "foo",
							Namespace: "pods",
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Env: []v1.EnvVar{
										{
											Name:  "BUILD_ID",
											Value: "0987654321",
										},
									},
								},
							},
						},
						Status: v1.PodStatus{
							Phase: v1.PodRunning,
						},
					},
				},
			},
			ExpectedState:       prowapi.PendingState,
			ExpectedNumPods:     map[string]int{"default": 1},
			ExpectedPendingTime: &pendingTime,
			ExpectedURL:         "foo/pending",
			ExpectedBuildID:     "0987654321",
		},
	}

	// Duplicate all tests for PlankV2
	for _, tc := range testcases {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		testcases = append(testcases, newTc)
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			totServ := httptest.NewServer(http.HandlerFunc(handleTot))
			defer totServ.Close()
			pm := make(map[string]v1.Pod)
			for _, pods := range tc.Pods {
				for i := range pods {
					pm[pods[i].ObjectMeta.Name] = pods[i]
				}
			}
			tc.PJ.Spec.Agent = prowapi.KubernetesAgent
			fakeProwJobClient := fakectrlruntimeclient.NewFakeClient(&tc.PJ)
			buildClients := map[string]ctrlruntimeclient.Client{}
			for alias, pods := range tc.Pods {
				var data []runtime.Object
				for i := range pods {
					pod := pods[i]
					data = append(data, &pod)
				}
				fakeClient := &createErroringClient{
					Client: fakectrlruntimeclient.NewFakeClient(data...),
					err:    tc.PodErr,
				}
				buildClients[alias] = fakeClient
			}
			if _, exists := buildClients[prowapi.DefaultClusterAlias]; !exists {
				buildClients[prowapi.DefaultClusterAlias] = &createErroringClient{
					Client: fakectrlruntimeclient.NewFakeClient(),
					err:    tc.PodErr,
				}
			}

			if !tc.IsV2 {
				c := Controller{
					prowJobClient: fakeProwJobClient,
					buildClients:  buildClients,
					log:           logrus.NewEntry(logrus.StandardLogger()),
					config:        newFakeConfigAgent(t, tc.MaxConcurrency).Config,
					totURL:        totServ.URL,
					pendingJobs:   make(map[string]int),
					clock:         fakeClock,
				}
				if tc.PendingJobs != nil {
					c.pendingJobs = tc.PendingJobs
				}

				if err := c.syncTriggeredJob(tc.PJ, pm); (err != nil) != tc.ExpectError {
					if tc.ExpectError {
						t.Errorf("for case %q expected an error, but got none", tc.Name)
					} else {
						t.Errorf("for case %q got an unexpected error: %v", tc.Name, err)
					}
					return
				}
			} else {
				for jobName, numJobsToCreate := range tc.PendingJobs {
					for i := 0; i < numJobsToCreate; i++ {
						if err := fakeProwJobClient.Create(context.Background(), &prowapi.ProwJob{
							ObjectMeta: metav1.ObjectMeta{
								Name:      fmt.Sprintf("%s-%d", jobName, i),
								Namespace: "prowjobs",
							},
							Spec: prowapi.ProwJobSpec{
								Agent: prowapi.KubernetesAgent,
								Job:   jobName,
							},
						}); err != nil {
							t.Fatalf("failed to create prowJob: %v", err)
						}
					}
				}
				r := &reconciler{
					pjClient:     fakeProwJobClient,
					buildClients: buildClients,
					log:          logrus.NewEntry(logrus.StandardLogger()),
					config:       newFakeConfigAgent(t, tc.MaxConcurrency).Config,
					totURL:       totServ.URL,
					clock:        fakeClock,
				}
				if _, err := r.syncTriggeredJob(tc.PJ.DeepCopy()); (err != nil) != tc.ExpectError {
					if tc.ExpectError {
						t.Errorf("for case %q expected an error, but got none", tc.Name)
					} else {
						t.Errorf("for case %q got an unexpected error: %v", tc.Name, err)
					}
					return
				}
			}

			actualProwJobs := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(context.Background(), actualProwJobs); err != nil {
				t.Errorf("for case %q could not list prowJobs from the client: %v", tc.Name, err)
			}
			actual := actualProwJobs.Items[0]
			if actual.Status.State != tc.ExpectedState {
				t.Errorf("expected state %q, got state %q", tc.ExpectedState, actual.Status.State)
			}
			if !reflect.DeepEqual(actual.Status.PendingTime, tc.ExpectedPendingTime) {
				t.Errorf("for case %q got pending time %v, expected %v", tc.Name, actual.Status.PendingTime, tc.ExpectedPendingTime)
			}
			if (actual.Status.PodName == "") && tc.ExpectedPodHasName {
				t.Errorf("for case %q got no pod name, expected one", tc.Name)
			}
			for alias, expected := range tc.ExpectedNumPods {
				actualPods := &v1.PodList{}
				if err := buildClients[alias].List(context.Background(), actualPods); err != nil {
					t.Errorf("for case %q could not list pods from the client: %v", tc.Name, err)
				}
				if got := len(actualPods.Items); got != expected {
					t.Errorf("for case %q got %d pods for alias %q, but expected %d", tc.Name, got, alias, expected)
				}
			}
			if actual.Complete() != tc.ExpectedComplete {
				t.Errorf("for case %q got wrong completion", tc.Name)
			}

		})
	}
}

func startTime(s time.Time) *metav1.Time {
	start := metav1.NewTime(s)
	return &start
}

func TestSyncPendingJob(t *testing.T) {

	type testCase struct {
		Name string

		PJ   prowapi.ProwJob
		Pods []v1.Pod
		Err  error
		IsV2 bool

		ExpectedState      prowapi.ProwJobState
		ExpectedNumPods    int
		ExpectedComplete   bool
		ExpectedCreatedPJs int
		ExpectedURL        string
	}
	var testcases = []testCase{
		{
			Name: "reset when pod goes missing",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-41",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.PostsubmitJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-41",
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 1,
			ExpectedURL:     "boop-41/pending",
		},
		{
			Name: "delete pod in unknown state",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-41",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-41",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-41",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodUnknown,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 0,
		},
		{
			Name: "succeeded pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type:    prowapi.BatchJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.SuccessState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "failed pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type: prowapi.PresubmitJob,
					Refs: &prowapi.Refs{
						Org: "kubernetes", Repo: "kubernetes",
						BaseRef: "baseref", BaseSHA: "basesha",
						Pulls: []prowapi.Pull{{Number: 100, Author: "me", SHA: "sha"}},
					},
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodFailed,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.FailureState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/failure",
		},
		{
			Name: "delete evicted pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: false,
			ExpectedState:    prowapi.PendingState,
			ExpectedNumPods:  0,
		},
		{
			Name: "don't delete evicted pod w/ error_on_eviction, complete PJ instead",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					ErrorOnEviction: true,
					PodSpec:         &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase:  v1.PodFailed,
						Reason: Evicted,
					},
				},
			},
			ExpectedComplete: true,
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  1,
			ExpectedURL:      "boop-42/error",
		},
		{
			Name: "running pod",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 1,
		},
		{
			Name: "pod changes url status",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "boop-42",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "boop-42",
					URL:     "boop-42/pending",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "boop-42",
						Namespace: "pods",
					},
					Status: v1.PodStatus{
						Phase: v1.PodSucceeded,
					},
				},
			},
			ExpectedComplete:   true,
			ExpectedState:      prowapi.SuccessState,
			ExpectedNumPods:    1,
			ExpectedCreatedPJs: 0,
			ExpectedURL:        "boop-42/success",
		},
		{
			Name: "unprocessable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jose",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Job:     "boop",
					Type:    prowapi.PostsubmitJob,
					PodSpec: &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
					Refs:    &prowapi.Refs{Org: "fejtaverse"},
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.PendingState,
				},
			},
			Err: &kapierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusUnprocessableEntity,
				Reason: metav1.StatusReasonInvalid,
			}},
			ExpectedState:    prowapi.ErrorState,
			ExpectedComplete: true,
			ExpectedURL:      "jose/error",
		},
		{
			Name: "stale pending prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nightmare",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "nightmare",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "nightmare",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podPendingTimeout)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodPending,
						StartTime: startTime(time.Now().Add(-podPendingTimeout)),
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  1,
			ExpectedComplete: true,
			ExpectedURL:      "nightmare/error",
		},
		{
			Name: "stale running prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "endless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "endless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "endless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podRunningTimeout)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodRunning,
						StartTime: startTime(time.Now().Add(-podRunningTimeout)),
					},
				},
			},
			ExpectedState:    prowapi.AbortedState,
			ExpectedNumPods:  0,
			ExpectedComplete: true,
			ExpectedURL:      "endless/aborted",
		},
		{
			Name: "stale unschedulable prow job",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "homeless",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "homeless",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "homeless",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podUnscheduledTimeout - time.Second)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			ExpectedState:    prowapi.ErrorState,
			ExpectedNumPods:  1,
			ExpectedComplete: true,
			ExpectedURL:      "homeless/error",
		},
		{
			Name: "scheduled, pending started more than podUnscheduledTimeout ago",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "slowpoke",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "slowpoke",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "slowpoke",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-podUnscheduledTimeout * 2)},
					},
					Status: v1.PodStatus{
						Phase:     v1.PodPending,
						StartTime: startTime(time.Now().Add(-podUnscheduledTimeout * 2)),
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 1,
		},
		{
			Name: "unscheduled, created less than podUnscheduledTimeout ago",
			PJ: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "just-waiting",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{},
				Status: prowapi.ProwJobStatus{
					State:   prowapi.PendingState,
					PodName: "just-waiting",
				},
			},
			Pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "just-waiting",
						Namespace:         "pods",
						CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Second)},
					},
					Status: v1.PodStatus{
						Phase: v1.PodPending,
					},
				},
			},
			ExpectedState:   prowapi.PendingState,
			ExpectedNumPods: 1,
		},
	}

	// Copy the tests for PlankV2
	for _, tc := range testcases {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		testcases = append(testcases, newTc)
	}

	for _, tc := range testcases {
		t.Run(tc.Name, func(t *testing.T) {
			totServ := httptest.NewServer(http.HandlerFunc(handleTot))
			defer totServ.Close()
			pm := make(map[string]v1.Pod)
			for i := range tc.Pods {
				pm[tc.Pods[i].ObjectMeta.Name] = tc.Pods[i]
			}
			fakeProwJobClient := fakectrlruntimeclient.NewFakeClient(&tc.PJ)
			var data []runtime.Object
			for i := range tc.Pods {
				pod := tc.Pods[i]
				data = append(data, &pod)
			}
			fakeClient := &createErroringClient{
				Client: fakectrlruntimeclient.NewFakeClient(data...),
				err:    tc.Err,
			}
			buildClients := map[string]ctrlruntimeclient.Client{
				prowapi.DefaultClusterAlias: fakeClient,
			}

			if !tc.IsV2 {
				c := Controller{
					prowJobClient: fakeProwJobClient,
					buildClients:  buildClients,
					log:           logrus.NewEntry(logrus.StandardLogger()),
					config:        newFakeConfigAgent(t, 0).Config,
					totURL:        totServ.URL,
					pendingJobs:   make(map[string]int),
					clock:         clock.RealClock{},
				}

				if err := c.syncPendingJob(tc.PJ, pm); err != nil {
					t.Fatalf("syncPendingJob failed: %v", err)
				}
			} else {
				r := &reconciler{
					pjClient:     fakeProwJobClient,
					buildClients: buildClients,
					log:          logrus.NewEntry(logrus.StandardLogger()),
					config:       newFakeConfigAgent(t, 0).Config,
					totURL:       totServ.URL,
					clock:        clock.RealClock{},
				}
				if err := r.syncPendingJob(&tc.PJ); err != nil {
					t.Fatalf("syncPendingJob failed: %v", err)
				}
			}

			actualProwJobs := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(context.Background(), actualProwJobs); err != nil {
				t.Errorf("for case %q could not list prowJobs from the client: %v", tc.Name, err)
			}
			if len(actualProwJobs.Items) != tc.ExpectedCreatedPJs+1 {
				t.Errorf("for case %q got %d created prowjobs", tc.Name, len(actualProwJobs.Items)-1)
			}
			actual := actualProwJobs.Items[0]
			if actual.Status.State != tc.ExpectedState {
				t.Errorf("for case %q got state %v", tc.Name, actual.Status.State)
			}
			actualPods := &v1.PodList{}
			if err := buildClients[prowapi.DefaultClusterAlias].List(context.Background(), actualPods); err != nil {
				t.Errorf("for case %q could not list pods from the client: %v", tc.Name, err)
			}
			if got := len(actualPods.Items); got != tc.ExpectedNumPods {
				t.Errorf("for case %q got %d pods, expected %d", tc.Name, len(actualPods.Items), tc.ExpectedNumPods)
			}
			if actual.Complete() != tc.ExpectedComplete {
				t.Errorf("for case %q got wrong completion", tc.Name)
			}

		})
	}
}

func TestOrderedJobs(t *testing.T) {
	totServ := httptest.NewServer(http.HandlerFunc(handleTot))
	defer totServ.Close()
	var pjs []prowapi.ProwJob

	// Add 3 jobs with incrementing timestamp
	for i := 0; i < 3; i++ {
		job := pjutil.NewProwJob(pjutil.PeriodicSpec(config.Periodic{
			JobBase: config.JobBase{
				Name:    fmt.Sprintf("ci-periodic-job-%d", i),
				Agent:   "kubernetes",
				Cluster: "trusted",
				Spec:    &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
			},
		}), nil, nil)
		job.ObjectMeta.CreationTimestamp = metav1.Time{
			Time: time.Now().Add(time.Duration(i) * time.Hour),
		}
		job.Namespace = "prowjobs"
		pjs = append(pjs, job)
	}
	expOut := []string{"ci-periodic-job-0", "ci-periodic-job-1", "ci-periodic-job-2"}

	for _, orders := range [][]int{
		{0, 1, 2},
		{1, 2, 0},
		{2, 0, 1},
	} {
		newPjs := make([]runtime.Object, 3)
		for i := 0; i < len(pjs); i++ {
			newPjs[i] = &pjs[orders[i]]
		}
		fakeProwJobClient := fakectrlruntimeclient.NewFakeClient(newPjs...)
		buildClients := map[string]ctrlruntimeclient.Client{
			"trusted": fakectrlruntimeclient.NewFakeClient(),
		}
		log := logrus.NewEntry(logrus.StandardLogger())
		c := Controller{
			prowJobClient: fakeProwJobClient,
			buildClients:  buildClients,
			log:           log,
			config:        newFakeConfigAgent(t, 0).Config,
			totURL:        totServ.URL,
			pendingJobs:   make(map[string]int),
			lock:          sync.RWMutex{},
			clock:         clock.RealClock{},
		}
		if err := c.Sync(); err != nil {
			t.Fatalf("Error on first sync: %v", err)
		}
		for i, name := range expOut {
			if c.pjs[i].Spec.Job != name {
				t.Errorf("Error in keeping order, want: '%s', got '%s'", name, c.pjs[i].Spec.Job)
			}
		}
	}
}

// TestPeriodic walks through the happy path of a periodic job.
func TestPeriodic(t *testing.T) {
	testCases := []struct {
		name string
	}{
		{"v1"},
		{"v2"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			per := config.Periodic{
				JobBase: config.JobBase{
					Name:    "ci-periodic-job",
					Agent:   "kubernetes",
					Cluster: "trusted",
					Spec:    &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
				},
			}

			totServ := httptest.NewServer(http.HandlerFunc(handleTot))
			defer totServ.Close()
			pj := pjutil.NewProwJob(pjutil.PeriodicSpec(per), nil, nil)
			pj.Namespace = "prowjobs"
			fakeProwJobClient := fakectrlruntimeclient.NewFakeClient(&pj)
			buildClients := map[string]ctrlruntimeclient.Client{
				prowapi.DefaultClusterAlias: fakectrlruntimeclient.NewFakeClient(),
				"trusted":                   fakectrlruntimeclient.NewFakeClient(),
			}
			logger := logrus.New()
			logger.SetLevel(logrus.DebugLevel)
			log := logrus.NewEntry(logger)
			var syncF func() error
			if tc.name == "v1" {
				c := Controller{
					prowJobClient: fakeProwJobClient,
					buildClients:  buildClients,
					log:           log,
					config:        newFakeConfigAgent(t, 0).Config,
					totURL:        totServ.URL,
					pendingJobs:   make(map[string]int),
					lock:          sync.RWMutex{},
					clock:         clock.RealClock{},
				}
				syncF = c.Sync
			} else {
				r := reconciler{
					pjClient:     fakeProwJobClient,
					buildClients: buildClients,
					log:          log,
					config:       newFakeConfigAgent(t, 0).Config,
					totURL:       totServ.URL,
					clock:        clock.RealClock{},
				}
				syncF = func() error {
					_, err := r.Reconcile(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "prowjobs", Name: pj.Name}})
					return err
				}
			}
			if err := syncF(); err != nil {
				t.Fatalf("Error on first sync: %v", err)
			}

			afterFirstSync := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(context.Background(), afterFirstSync); err != nil {
				t.Fatalf("could not list prowJobs from the client: %v", err)
			}
			if len(afterFirstSync.Items) != 1 {
				t.Fatalf("saw %d prowjobs after sync, not 1", len(afterFirstSync.Items))
			}
			if len(afterFirstSync.Items[0].Spec.PodSpec.Containers) != 1 || afterFirstSync.Items[0].Spec.PodSpec.Containers[0].Name != "test-name" {
				t.Fatalf("Sync step updated the pod spec: %#v", afterFirstSync.Items[0].Spec.PodSpec)
			}
			podsAfterSync := &v1.PodList{}
			if err := buildClients["trusted"].List(context.Background(), podsAfterSync); err != nil {
				t.Fatalf("could not list pods from the client: %v", err)
			}
			if len(podsAfterSync.Items) != 1 {
				t.Fatalf("expected exactly one pod, got %d", len(podsAfterSync.Items))
			}
			if len(podsAfterSync.Items[0].Spec.Containers) != 1 {
				t.Fatal("Wiped container list.")
			}
			if len(podsAfterSync.Items[0].Spec.Containers[0].Env) == 0 {
				t.Fatal("Container has no env set.")
			}
			if err := syncF(); err != nil {
				t.Fatalf("Error on second sync: %v", err)
			}
			podsAfterSecondSync := &v1.PodList{}
			if err := buildClients["trusted"].List(context.Background(), podsAfterSecondSync); err != nil {
				t.Fatalf("could not list pods from the client: %v", err)
			}
			if len(podsAfterSecondSync.Items) != 1 {
				t.Fatalf("Wrong number of pods after second sync: %d", len(podsAfterSecondSync.Items))
			}
			update := podsAfterSecondSync.Items[0].DeepCopy()
			update.Status.Phase = v1.PodSucceeded
			if err := buildClients["trusted"].Update(context.Background(), update); err != nil {
				t.Fatalf("could not update pod to be succeeded: %v", err)
			}
			if err := syncF(); err != nil {
				t.Fatalf("Error on third sync: %v", err)
			}
			afterThirdSync := &prowapi.ProwJobList{}
			if err := fakeProwJobClient.List(context.Background(), afterThirdSync); err != nil {
				t.Fatalf("could not list prowJobs from the client: %v", err)
			}
			if len(afterThirdSync.Items) != 1 {
				t.Fatalf("Wrong number of prow jobs: %d", len(afterThirdSync.Items))
			}
			if !afterThirdSync.Items[0].Complete() {
				t.Fatal("Prow job didn't complete.")
			}
			if afterThirdSync.Items[0].Status.State != prowapi.SuccessState {
				t.Fatalf("Should be success: %v", afterThirdSync.Items[0].Status.State)
			}
			if err := syncF(); err != nil {
				t.Fatalf("Error on fourth sync: %v", err)
			}
		})
	}
}

func TestMaxConcurrencyWithNewlyTriggeredJobs(t *testing.T) {
	type testCase struct {
		Name         string
		PJs          []prowapi.ProwJob
		PendingJobs  map[string]int
		IsV2         bool
		ExpectedPods int
	}

	tests := []testCase{
		{
			Name: "avoid starting a triggered job",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "first",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 1,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "second",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 1,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  make(map[string]int),
			ExpectedPods: 1,
		},
		{
			Name: "both triggered jobs can start",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "first",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 2,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "second",
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 2,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  make(map[string]int),
			ExpectedPods: 2,
		},
		{
			Name: "no triggered job can start",
			PJs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "first",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 5,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "second",
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           prowapi.PostsubmitJob,
						MaxConcurrency: 5,
						PodSpec:        &v1.PodSpec{Containers: []v1.Container{{Name: "test-name", Env: []v1.EnvVar{}}}},
						Refs:           &prowapi.Refs{Org: "fejtaverse"},
					},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					},
				},
			},
			PendingJobs:  map[string]int{"test-bazel-build": 5},
			ExpectedPods: 0,
		},
	}
	// Duplicate all tests for PlankV2
	for _, tc := range tests {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		tests = append(tests, newTc)
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			jobs := make(chan prowapi.ProwJob, len(test.PJs))
			for _, pj := range test.PJs {
				jobs <- pj
			}
			close(jobs)

			var prowJobs []runtime.Object
			for i := range test.PJs {
				test.PJs[i].Namespace = "prowjobs"
				test.PJs[i].Spec.Agent = prowapi.KubernetesAgent
				prowJobs = append(prowJobs, &test.PJs[i])
			}
			fakeProwJobClient := fakectrlruntimeclient.NewFakeClient(prowJobs...)
			buildClients := map[string]ctrlruntimeclient.Client{
				prowapi.DefaultClusterAlias: fakectrlruntimeclient.NewFakeClient(),
			}
			if !test.IsV2 {
				c := Controller{
					prowJobClient: fakeProwJobClient,
					buildClients:  buildClients,
					log:           logrus.NewEntry(logrus.StandardLogger()),
					config:        newFakeConfigAgent(t, 0).Config,
					pendingJobs:   test.PendingJobs,
					clock:         clock.RealClock{},
				}

				errors := make(chan error, len(test.PJs))
				pm := make(map[string]v1.Pod)

				syncProwJobs(c.log, c.syncTriggeredJob, 20, jobs, errors, pm)
			} else {
				for jobName, numJobsToCreate := range test.PendingJobs {
					for i := 0; i < numJobsToCreate; i++ {
						if err := fakeProwJobClient.Create(context.Background(), &prowapi.ProwJob{
							ObjectMeta: metav1.ObjectMeta{
								Name:      fmt.Sprintf("%s-%d", jobName, i),
								Namespace: "prowjobs",
							},
							Spec: prowapi.ProwJobSpec{
								Agent: prowapi.KubernetesAgent,
								Job:   jobName,
							},
						}); err != nil {
							t.Fatalf("failed to create prowJob: %v", err)
						}
					}
				}
				r := &reconciler{
					pjClient: &indexingClient{
						Client:     fakeProwJobClient,
						indexFuncs: map[string]ctrlruntimeclient.IndexerFunc{prowJobIndexName: prowJobIndexer("prowjobs")},
					},
					buildClients: buildClients,
					log:          logrus.NewEntry(logrus.StandardLogger()),
					config:       newFakeConfigAgent(t, 0).Config,
					clock:        clock.RealClock{},
				}
				for _, job := range test.PJs {
					request := reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      job.Name,
						Namespace: job.Namespace,
					}}
					if _, err := r.Reconcile(request); err != nil {
						t.Fatalf("failed to reconcile job %s: %v", request.String(), err)
					}
				}
			}

			podsAfterSync := &v1.PodList{}
			if err := buildClients[prowapi.DefaultClusterAlias].List(context.Background(), podsAfterSync); err != nil {
				t.Fatalf("could not list pods from the client: %v", err)
			}
			if len(podsAfterSync.Items) != test.ExpectedPods {
				t.Errorf("expected pods: %d, got: %d", test.ExpectedPods, len(podsAfterSync.Items))
			}
		})
	}
}

func TestMaxConcurency(t *testing.T) {
	type testCase struct {
		Name             string
		ProwJob          prowapi.ProwJob
		ExistingProwJobs []prowapi.ProwJob
		PendingJobs      map[string]int
		IsV2             bool

		ExpectedResult bool
	}
	testCases := []testCase{
		{
			Name:           "Max concurency 0 always runs",
			ProwJob:        prowapi.ProwJob{Spec: prowapi.ProwJobSpec{MaxConcurrency: 0}},
			ExpectedResult: true,
		},
		{
			Name: "Num pending exceeds max concurrency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Now()},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj"}},
			PendingJobs:    map[string]int{"my-pj": 10},
			ExpectedResult: false,
		},
		{
			Name: "Num pending plus older instances equals max concurency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj"},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "prowjobs"},
					Spec:       prowapi.ProwJobSpec{Agent: prowapi.KubernetesAgent, Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					}},
			},
			PendingJobs:    map[string]int{"my-pj": 9},
			ExpectedResult: false,
		},
		{
			Name: "Num pending plus older instances exceeds max concurency",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 10,
					Job:            "my-pj"},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					}},
			},
			PendingJobs:    map[string]int{"my-pj": 10},
			ExpectedResult: false,
		},
		{
			Name: "Have other jobs that are newer, can execute",
			ProwJob: prowapi.ProwJob{
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 1,
					Job:            "my-pj"},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: metav1.Now(),
					},
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						State: prowapi.TriggeredState,
					}},
			},
			ExpectedResult: true,
		},
		{
			Name: "Have older jobs that are not triggered, can execute",
			ProwJob: prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Now(),
				},
				Spec: prowapi.ProwJobSpec{
					MaxConcurrency: 2,
					Job:            "my-pj"},
			},
			ExistingProwJobs: []prowapi.ProwJob{
				{
					Spec: prowapi.ProwJobSpec{Job: "my-pj"},
					Status: prowapi.ProwJobStatus{
						CompletionTime: &[]metav1.Time{{}}[0],
					}},
			},
			PendingJobs:    map[string]int{"my-pj": 1},
			ExpectedResult: true,
		},
	}

	// Duplicate all tests for PlankV2
	for _, tc := range testCases {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		testCases = append(testCases, newTc)
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {

			if tc.PendingJobs == nil {
				tc.PendingJobs = map[string]int{}
			}
			var prowJobs []runtime.Object
			for i := range tc.ExistingProwJobs {
				tc.ExistingProwJobs[i].Namespace = "prowjobs"
				prowJobs = append(prowJobs, &tc.ExistingProwJobs[i])
			}
			buildClients := map[string]ctrlruntimeclient.Client{}
			logrus.SetLevel(logrus.DebugLevel)

			var result bool
			if !tc.IsV2 {
				c := Controller{
					pjs:          tc.ExistingProwJobs,
					buildClients: buildClients,
					log:          logrus.NewEntry(logrus.StandardLogger()),
					config:       newFakeConfigAgent(t, 0).Config,
					pendingJobs:  tc.PendingJobs,
					clock:        clock.RealClock{},
				}
				result = c.canExecuteConcurrently(&tc.ProwJob)
			} else {
				for jobName, numJobsToCreate := range tc.PendingJobs {
					for i := 0; i < numJobsToCreate; i++ {
						prowJobs = append(prowJobs, &prowapi.ProwJob{
							ObjectMeta: metav1.ObjectMeta{
								Name:      fmt.Sprintf("%s-%d", jobName, i),
								Namespace: "prowjobs",
							},
							Spec: prowapi.ProwJobSpec{
								Agent: prowapi.KubernetesAgent,
								Job:   jobName,
							},
						})
					}
				}
				r := &reconciler{
					pjClient: &indexingClient{
						Client:     fakectrlruntimeclient.NewFakeClient(prowJobs...),
						indexFuncs: map[string]ctrlruntimeclient.IndexerFunc{prowJobIndexName: prowJobIndexer("prowjobs")},
					},
					buildClients: buildClients,
					log:          logrus.NewEntry(logrus.StandardLogger()),
					config:       newFakeConfigAgent(t, 0).Config,
					clock:        clock.RealClock{},
				}
				var err error
				tc.ProwJob.UID = types.UID("under-test")
				result, err = r.canExecuteConcurrently(&tc.ProwJob)
				if err != nil {
					t.Fatalf("canExecuteConcurrently: %v", err)
				}
			}

			if result != tc.ExpectedResult {
				t.Errorf("Expected result to be %t but was %t", tc.ExpectedResult, result)
			}
		})
	}

}

type patchTrackingFakeClient struct {
	ctrlruntimeclient.Client
	patched sets.String
}

func (c *patchTrackingFakeClient) Patch(ctx context.Context, obj runtime.Object, patch ctrlruntimeclient.Patch, opts ...ctrlruntimeclient.PatchOption) error {
	if c.patched == nil {
		c.patched = sets.NewString()
	}
	metaObject, ok := obj.(metav1.Object)
	if !ok {
		return errors.New("Object is no metav1.Object")
	}
	c.patched.Insert(metaObject.GetName())
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type deleteTrackingFakeClient struct {
	deleteError error
	ctrlruntimeclient.Client
	deleted sets.String
}

func (c *deleteTrackingFakeClient) Delete(ctx context.Context, obj runtime.Object, opts ...ctrlruntimeclient.DeleteOption) error {
	if c.deleteError != nil {
		return c.deleteError
	}
	if c.deleted == nil {
		c.deleted = sets.String{}
	}
	metaObject, ok := obj.(metav1.Object)
	if !ok {
		return errors.New("object is not a metav1.Object")
	}
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	c.deleted.Insert(metaObject.GetName())
	return nil
}

type createErroringClient struct {
	ctrlruntimeclient.Client
	err error
}

func (c *createErroringClient) Create(ctx context.Context, obj runtime.Object, opts ...ctrlruntimeclient.CreateOption) error {
	if c.err != nil {
		return c.err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestSyncAbortedJob(t *testing.T) {
	t.Parallel()

	type testCase struct {
		Name           string
		Pod            *v1.Pod
		DeleteError    error
		IsV2           bool
		ExpectSyncFail bool
		ExpectDelete   bool
		ExpectComplete bool
	}

	testCases := []testCase{
		{
			Name:           "Pod is deleted",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			ExpectDelete:   true,
			ExpectComplete: true,
		},
		{
			Name:           "No pod there",
			ExpectDelete:   false,
			ExpectComplete: true,
		},
		{
			Name:           "NotFound on delete is tolerated",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			DeleteError:    kapierrors.NewNotFound(schema.GroupResource{}, "my-pj"),
			ExpectDelete:   false,
			ExpectComplete: true,
		},
		{
			Name:           "Failed delete does not set job to completed",
			Pod:            &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-pj"}},
			DeleteError:    errors.New("erroring as requested"),
			ExpectSyncFail: true,
			ExpectDelete:   false,
			ExpectComplete: false,
		},
	}

	// Duplicate all tests for PlankV2
	for _, tc := range testCases {
		if tc.IsV2 {
			continue
		}
		newTc := deepcopy.Copy(tc).(testCase)
		newTc.Name = "[PlankV2] " + newTc.Name
		newTc.IsV2 = true
		testCases = append(testCases, newTc)
	}

	const cluster = "cluster"
	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {

			pj := &prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-pj",
				},
				Spec: prowapi.ProwJobSpec{
					Cluster: cluster,
				},
				Status: prowapi.ProwJobStatus{
					State: prowapi.AbortedState,
				},
			}

			var pods []runtime.Object
			var podMap map[string]v1.Pod
			if tc.Pod != nil {
				pods = append(pods, tc.Pod)
				podMap = map[string]v1.Pod{pj.Name: *tc.Pod}
			}
			podClient := &deleteTrackingFakeClient{
				deleteError: tc.DeleteError,
				Client:      fakectrlruntimeclient.NewFakeClient(pods...),
			}

			pjClient := fakectrlruntimeclient.NewFakeClient(pj)
			var sync func() error
			if !tc.IsV2 {
				c := &Controller{
					log:           logrus.NewEntry(logrus.New()),
					prowJobClient: pjClient,
					buildClients:  map[string]ctrlruntimeclient.Client{cluster: podClient},
				}
				sync = func() error {
					return c.syncAbortedJob(*pj, podMap)
				}
			} else {
				r := &reconciler{
					log:          logrus.NewEntry(logrus.New()),
					config:       func() *config.Config { return &config.Config{} },
					pjClient:     pjClient,
					buildClients: map[string]ctrlruntimeclient.Client{cluster: podClient},
				}
				sync = func() error {
					res, err := r.reconcile(pj)
					if res != nil {
						err = utilerrors.NewAggregate([]error{err, fmt.Errorf("expected reconcile.Result to be nil, was %v", res)})
					}
					return err
				}
			}

			if err := sync(); (err != nil) != tc.ExpectSyncFail {
				t.Fatalf("sync failed: %v, expected it to fail: %t", err, tc.ExpectSyncFail)
			}

			if err := pjClient.Get(context.Background(), types.NamespacedName{Name: pj.Name}, pj); err != nil {
				t.Fatalf("failed to get job from client: %v", err)
			}
			if pj.Complete() != tc.ExpectComplete {
				t.Errorf("expected complete: %t, got complete: %t", tc.ExpectComplete, pj.Complete())
			}

			if tc.ExpectDelete != podClient.deleted.Has(pj.Name) {
				t.Errorf("expected delete: %t, got delete: %t", tc.ExpectDelete, podClient.deleted.Has(pj.Name))
			}
		})
	}
}

type indexingClient struct {
	ctrlruntimeclient.Client
	indexFuncs map[string]ctrlruntimeclient.IndexerFunc
}

func (c *indexingClient) List(ctx context.Context, list runtime.Object, opts ...ctrlruntimeclient.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}

	listOpts := &ctrlruntimeclient.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(listOpts)
	}

	if listOpts.FieldSelector == nil {
		return nil
	}

	if n := len(listOpts.FieldSelector.Requirements()); n == 0 {
		return nil
	} else if n > 1 {
		return fmt.Errorf("the indexing client supports at most one field selector requirement, got %d", n)
	}

	indexKey := listOpts.FieldSelector.Requirements()[0].Field
	if indexKey == "" {
		return nil
	}

	indexFunc, ok := c.indexFuncs[indexKey]
	if !ok {
		return fmt.Errorf("no index with key %q found", indexKey)
	}

	pjList, ok := list.(*prowapi.ProwJobList)
	if !ok {
		return errors.New("indexes are only supported for ProwJobLists")
	}

	result := prowapi.ProwJobList{}
	for _, pj := range pjList.Items {
		for _, indexVal := range indexFunc(&pj) {
			logrus.Infof("indexVal: %q, requirementVal: %q, match: %t, name: %s", indexVal, listOpts.FieldSelector.Requirements()[0].Value, indexVal == listOpts.FieldSelector.Requirements()[0].Value, pj.Name)
			if indexVal == listOpts.FieldSelector.Requirements()[0].Value {
				result.Items = append(result.Items, pj)
			}
		}
	}

	*pjList = result
	return nil
}
