package tide

import (
	"context"
	"os"
	"testing"

	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"
	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestParallelCreate(t *testing.T) {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("failed to build config: %v", err)
	}

	mgr, err := crmanager.New(cfg, crmanager.Options{})
	if err != nil {
		t.Fatalf("failed to construct mgr: %v", err)
	}

	ctx := context.Background()
	go func() {
		if err := mgr.Start(ctx.Done()); err != nil {
			t.Fatalf("failed to start mgr: %v", err)
		}
	}()
	if synced := mgr.GetCache().WaitForCacheSync(ctx.Done()); !synced {
		t.Fatal("failed to wait for cache sync:")
	}
	// Create a cache
	if err := mgr.GetClient().List(ctx, &prowapi.ProwJobList{}); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	if synced := mgr.GetCache().WaitForCacheSync(ctx.Done()); !synced {
		t.Fatal("failed to wait for cache sync:")
	}

	c := &Controller{
		config: func() *config.Config {
			return &config.Config{
				ProwConfig: config.ProwConfig{ProwJobNamespace: "alvaro-test"},
			}
		},
		ctx:           ctx,
		prowJobClient: mgr.GetClient(),
		logger:        logrus.NewEntry(logrus.StandardLogger()),
	}

	sp := subpool{
		org:    "org",
		repo:   "repo",
		branch: "Branch",
		sha:    "123",
	}
	ps := []config.Presubmit{{}}
	prs := []PullRequest{
		{
			Number:     githubql.Int(1),
			Author:     struct{ Login githubql.String }{Login: githubql.String("author")},
			HeadRefOID: githubql.String("123"),
		},
		{
			Number:     githubql.Int(2),
			Author:     struct{ Login githubql.String }{Login: githubql.String("author")},
			HeadRefOID: githubql.String("123"),
		},
	}

	c1 := make(chan struct{})
	c2 := make(chan struct{})
	c3 := make(chan struct{})
	c4 := make(chan struct{})
	go func() {
		if err := c.trigger(sp, ps, prs); err != nil {
			t.Errorf("trigger 1: %v", err)
		}
		close(c1)
	}()
	go func() {
		if err := c.trigger(sp, ps, prs); err != nil {
			t.Errorf("trigger 2: %v", err)
		}
		close(c2)
	}()
	go func() {
		if err := c.prowJobClient.Create(ctx, pj("one")); err != nil {
			t.Errorf("create one: %v", err)
		}
		close(c3)
	}()
	go func() {
		if err := c.prowJobClient.Create(ctx, pj("two")); err != nil {
			t.Errorf("create two: %v", err)
		}
		close(c4)
	}()
	<-c1
	<-c2
	<-c3
	<-c4

}

func pj(name string) *prowapi.ProwJob {
	pj := &prowapi.ProwJob{}
	pj.Name = name
	pj.Namespace = "alvaro-test"
	pj.Annotations = map[string]string{"hello": "world"}
	return pj
}
