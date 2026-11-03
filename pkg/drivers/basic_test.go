package drivers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAnnotationProjectReadsOnlyTheNamespace(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "team-a", Annotations: map[string]string{ProjectAnnotation: "project-team-a"}}}
	p := &annotationProjectDriver{
		k8s:       fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(ns).Build(),
		projectID: "project-default",
	}
	ctx := context.Background()

	// A pod naming another project must not be able to move itself there.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a",
		Annotations: map[string]string{ProjectAnnotation: "project-victim"}}}
	if got, err := p.ProjectFor(ctx, pod); err != nil || got != "project-team-a" {
		t.Errorf("pod project = %q, %v; want project-team-a from its namespace", got, err)
	}

	// A stub Namespace carrying only its name still resolves the annotation.
	stub := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	if got, err := p.ProjectFor(ctx, stub); err != nil || got != "project-team-a" {
		t.Errorf("namespace project = %q, %v; want project-team-a", got, err)
	}

	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}}
	p.k8s = fake.NewClientBuilder().WithScheme(policyTestScheme(t)).WithObjects(ns, other).Build()
	if got, err := p.ProjectFor(ctx, other); err != nil || got != "project-default" {
		t.Errorf("unannotated namespace project = %q, %v; want project-default", got, err)
	}
}
