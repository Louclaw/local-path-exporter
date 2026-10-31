package mapping

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseDirNameRightSplit(t *testing.T) {
	pv, ns, name, ok := ParseDirName("pvc-uid_ns_data")
	if !ok || pv != "pvc-uid" || ns != "ns" || name != "data" {
		t.Fatalf("got %q %q %q ok=%v", pv, ns, name, ok)
	}
	pv, ns, name, ok = ParseDirName("pvc-uid_extra_my-ns_my-pvc")
	if !ok || pv != "pvc-uid_extra" || ns != "my-ns" || name != "my-pvc" {
		t.Fatalf("right split got %q %q %q ok=%v", pv, ns, name, ok)
	}
	if _, _, _, ok := ParseDirName("foo_ns_data"); ok {
		t.Fatal("prefix must be pvc-")
	}
	if _, _, _, ok := ParseDirName("pvc-_ns_data"); ok {
		t.Fatal("empty id must be rejected")
	}
	if _, _, _, ok := ParseDirName("pvc-uid_ns"); ok {
		t.Fatal("two fields must be rejected")
	}
	if _, _, _, ok := ParseDirName("pvc-uid_ns_"); ok {
		t.Fatal("empty name must be rejected")
	}
}

func TestResolveUsesClaimAndPVCRequest(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pv := hostPathPV("pvc-1", "node-a", root+"/pvc-1_ns_other", "ns", "data")
	pv.Spec.Capacity = corev1.ResourceList{
		corev1.ResourceStorage: resource.MustParse("10Gi"),
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}
	other := hostPathPV("pvc-2", "node-b", root+"/pvc-2_ns_skip", "ns", "skip")

	got := Resolve([]*corev1.PersistentVolume{pv, other}, []*corev1.PersistentVolumeClaim{pvc}, "node-a", root, []string{"pvc-1_ns_other", "pvc-2_ns_skip"})
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	data := got[0]
	if data.Name != "data" {
		data = got[1]
	}
	if data.Namespace != "ns" || data.Name != "data" || !data.FromPV {
		t.Fatalf("identity: %#v", got)
	}
	if !data.CapacityFromPVC || data.Capacity != 2*(1<<30) {
		t.Fatalf("capacity = %d fromPVC=%v, want PVC request 2Gi not PV capacity", data.Capacity, data.CapacityFromPVC)
	}
	if data.PV != "pvc-1" || data.Node != "node-a" {
		t.Fatalf("labels pv=%s node=%s", data.PV, data.Node)
	}
	if data.Path != root+"/pvc-1_ns_other" {
		t.Fatalf("path %s", data.Path)
	}
}

func TestResolveLocalPathPreferred(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pvc-1",
			Annotations: map[string]string{SelectedNodeAnnotation: "node-a"},
		},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: "ns", Name: "data"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local:    &corev1.LocalVolumeSource{Path: root + "/pvc-1_ns_data"},
				HostPath: &corev1.HostPathVolumeSource{Path: root + "/ignored"},
			},
		},
	}
	got := Resolve([]*corev1.PersistentVolume{pv}, nil, "node-a", root, nil)
	if len(got) != 1 || got[0].Path != root+"/pvc-1_ns_data" {
		t.Fatalf("got %#v", got)
	}
}

func TestResolveHostPathWhenLocalOutside(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{SelectedNodeAnnotation: "node-a"},
		},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: "ns", Name: "data"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local:    &corev1.LocalVolumeSource{Path: "/elsewhere/data"},
				HostPath: &corev1.HostPathVolumeSource{Path: root + "/pvc-1_ns_data"},
			},
		},
	}
	got := Resolve([]*corev1.PersistentVolume{pv}, nil, "node-a", root, nil)
	if len(got) != 1 || got[0].Path != root+"/pvc-1_ns_data" {
		t.Fatalf("got %#v", got)
	}
}

func TestResolveRejectsEscape(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pv := hostPathPV("pvc-1", "node-a", root+"/../../etc/passwd", "ns", "data")
	if got := Resolve([]*corev1.PersistentVolume{pv}, nil, "node-a", root, nil); len(got) != 0 {
		t.Fatalf("escaped path included: %#v", got)
	}
}

func TestResolveFallbackWhenNoPV(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "logs"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	got := Resolve(nil, []*corev1.PersistentVolumeClaim{pvc}, "node-a", root, []string{
		"pvc-abc_def_team_logs",
		"lost+found",
		"not-a-pvc",
	})
	if len(got) != 1 {
		t.Fatalf("got %#v", got)
	}
	if got[0].FromPV || got[0].Namespace != "team" || got[0].Name != "logs" || !got[0].CapacityFromPVC || got[0].Capacity != 1<<30 {
		t.Fatalf("fallback: %#v", got[0])
	}
	if got[0].PV != "pvc-abc_def" || got[0].Node != "node-a" {
		t.Fatalf("fallback pv label: %#v", got[0])
	}
}

func TestResolveWithoutStorageRequest(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "logs"},
	}
	got := Resolve(nil, []*corev1.PersistentVolumeClaim{pvc}, "node-a", root, []string{"pvc-abc_team_logs"})
	if len(got) != 1 || got[0].CapacityFromPVC || got[0].Capacity != 0 {
		t.Fatalf("missing request must not publish a zero PVC capacity: %#v", got)
	}
}

func TestResolvePVWinsOverDirName(t *testing.T) {
	const root = "/var/lib/rancher/k3s/storage"
	dir := "pvc-abc_wrong_wrong"
	pv := hostPathPV("pvc-abc", "node-a", root+"/"+dir, "real", "claim")
	got := Resolve([]*corev1.PersistentVolume{pv}, nil, "node-a", root, []string{dir})
	if len(got) != 1 || got[0].Namespace != "real" || got[0].Name != "claim" || !got[0].FromPV || got[0].PV != "pvc-abc" {
		t.Fatalf("got %#v", got)
	}
}

func TestIndexedNodes(t *testing.T) {
	pv := hostPathPV("pvc-1", "node-a", "/var/lib/rancher/k3s/storage/x", "ns", "c")
	if got := IndexedNodes(pv); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("annotation index: %v", got)
	}
	bare := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{NodeAffinity: affinity(corev1.LabelHostname, "node-b")}}
	if got := IndexedNodes(bare); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("affinity index: %v", got)
	}
	if IndexedNodes(&corev1.PersistentVolume{}) != nil {
		t.Fatal("unpinned PV must not be indexed")
	}
}

func TestOnNodeAffinityWithoutAnnotation(t *testing.T) {
	pv := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: affinity(corev1.LabelHostname, "node-a"),
		},
	}
	if !OnNode(pv, "node-a") || OnNode(pv, "node-b") {
		t.Fatal("hostname affinity")
	}

	custom := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: affinity("custom.example/node", "node-a"),
		},
	}
	if !OnNode(custom, "node-a") {
		t.Fatal("custom key with the node name should match")
	}

	fields := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchFields: []corev1.NodeSelectorRequirement{{
							Key:      "metadata.name",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"node-a"},
						}},
					}},
				},
			},
		},
	}
	if !OnNode(fields, "node-a") || OnNode(fields, "node-b") {
		t.Fatal("matchFields metadata.name")
	}

	extra := &corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}},
							{Key: "disk", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"}},
						},
					}},
				},
			},
		},
	}
	if OnNode(extra, "node-a") {
		t.Fatal("unknown extra expression must not match")
	}

	annotated := hostPathPV("p", "node-b", "/var/lib/rancher/k3s/storage/x", "ns", "c")
	annotated.Spec.NodeAffinity = affinity(corev1.LabelHostname, "node-a")
	if OnNode(annotated, "node-a") || !OnNode(annotated, "node-b") {
		t.Fatal("annotation must win over affinity")
	}
}

func hostPathPV(name, node, path, ns, claim string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{SelectedNodeAnnotation: node},
		},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: ns, Name: claim},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: path},
			},
			NodeAffinity: affinity(corev1.LabelHostname, node),
		},
	}
}

func affinity(key, node string) *corev1.VolumeNodeAffinity {
	return &corev1.VolumeNodeAffinity{
		Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      key,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{node},
				}},
			}},
		},
	}
}
