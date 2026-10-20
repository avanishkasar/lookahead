package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/avanishkasar/lookahead/api/v1alpha1"
)

func TestStateAndPage(t *testing.T) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, extv1beta1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&extv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool-a"},
			Spec:       extv1beta1.SandboxWarmPoolSpec{Replicas: new(int32(3)), TemplateRef: extv1beta1.SandboxTemplateRef{Name: "tmpl"}},
			Status:     extv1beta1.SandboxWarmPoolStatus{Replicas: 3, ReadyReplicas: 2},
		}).Build()

	now := time.Unix(1_000_000, 0)
	d := New(c, Config{Namespace: "default", Now: func() time.Time { return now }})
	d.Sample(context.Background())
	now = now.Add(time.Second)
	d.Sample(context.Background())

	srv := httptest.NewServer(d.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.Pools) != 1 || st.Pools[0].Snapshot.Pool != "pool-a" || len(st.Pools[0].Samples) != 2 {
		t.Fatalf("state = %+v", st)
	}
	if !strings.Contains(st.Pools[0].Brief, "2 of 3 warm sandboxes ready") || st.WindowSec != 600 {
		t.Errorf("brief %q window %d", st.Pools[0].Brief, st.WindowSec)
	}

	page, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if ct := page.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
	if nf, _ := http.Get(srv.URL + "/nope"); nf.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status %d", nf.StatusCode)
	}
}
