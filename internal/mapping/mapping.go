// Package mapping links a local-path directory to a PVC.
// The informer view (PV path, node affinity, claimRef) is authoritative.
// The directory name pvc-<id>_<namespace>_<pvc> is only a fallback.
package mapping

import (
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Annotation set by rancher/local-path-provisioner to the real node name.
const SelectedNodeAnnotation = "local.path.provisioner/selected-node"

// Volume is one PVC directory to measure on this node.
type Volume struct {
	Path            string
	Namespace       string
	Name            string
	PV              string
	Node            string
	Capacity        int64
	CapacityFromPVC bool
	FromPV          bool
}

// ParseDirName splits pvc-<id>_<namespace>_<name> from the right.
// The returned PV name is the pvc-<id> prefix. The id may contain underscores;
// namespace and name are the last two fields.
func ParseDirName(name string) (pv, namespace, pvc string, ok bool) {
	rest, pvc, ok1 := cutLast(name, "_")
	id, namespace, ok2 := cutLast(rest, "_")
	if !ok1 || !ok2 || !validIdent(namespace) || !validIdent(pvc) || !validIdent(id) {
		return "", "", "", false
	}
	if !strings.HasPrefix(id, "pvc-") || len(id) <= len("pvc-") {
		return "", "", "", false
	}
	return id, namespace, pvc, true
}

// IndexedNodes lists the nodes an informer index should file this PV under.
// The watch stays cluster-wide: node placement is not a field selector.
func IndexedNodes(pv *corev1.PersistentVolume) []string {
	if pv == nil {
		return nil
	}
	if ann := pv.Annotations[SelectedNodeAnnotation]; ann != "" {
		if OnNode(pv, ann) {
			return []string{ann}
		}
		return nil
	}
	aff := pv.Spec.NodeAffinity
	if aff == nil || aff.Required == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	consider := func(values []string) {
		for _, name := range values {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			if !OnNode(pv, name) {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	for _, term := range aff.Required.NodeSelectorTerms {
		for _, exp := range term.MatchExpressions {
			if exp.Operator == corev1.NodeSelectorOpIn {
				consider(exp.Values)
			}
		}
		for _, exp := range term.MatchFields {
			if exp.Operator == corev1.NodeSelectorOpIn {
				consider(exp.Values)
			}
		}
	}
	sort.Strings(out)
	return out
}

// OnNode reports whether the PV is pinned to nodeName.
// The local-path selected-node annotation wins when it is set.
func OnNode(pv *corev1.PersistentVolume, nodeName string) bool {
	if pv == nil || nodeName == "" {
		return false
	}
	if ann := pv.Annotations[SelectedNodeAnnotation]; ann != "" {
		return ann == nodeName
	}
	aff := pv.Spec.NodeAffinity
	if aff == nil || aff.Required == nil {
		return false
	}
	for _, term := range aff.Required.NodeSelectorTerms {
		if termMatches(term, nodeName) {
			return true
		}
	}
	return false
}

// Resolve builds the per-node volume list.
// dirNames are the immediate child directory names of storagePath.
func Resolve(pvs []*corev1.PersistentVolume, pvcs []*corev1.PersistentVolumeClaim, nodeName, storagePath string, dirNames []string) []Volume {
	storagePath = filepath.Clean(storagePath)
	caps := capacities(pvcs)
	byKey := map[string]Volume{}

	for _, pv := range pvs {
		if !OnNode(pv, nodeName) {
			continue
		}
		pth := pathWithin(pv, storagePath)
		if pth == "" {
			continue
		}
		pvName, ns, name, ok := identity(pv, filepath.Base(pth))
		if !ok {
			continue
		}
		key := ns + "/" + name
		if prev, exists := byKey[key]; exists && prev.FromPV {
			continue
		}
		byKey[key] = volume(pth, ns, name, pvName, nodeName, caps, true)
	}

	covered := map[string]struct{}{}
	for _, v := range byKey {
		covered[v.Path] = struct{}{}
	}

	for _, dir := range dirNames {
		if !filepath.IsLocal(dir) {
			continue
		}
		pth := filepath.Join(storagePath, dir)
		if _, ok := covered[pth]; ok {
			continue
		}
		pvName, ns, name, ok := ParseDirName(dir)
		if !ok {
			continue
		}
		key := ns + "/" + name
		if _, exists := byKey[key]; exists {
			continue
		}
		byKey[key] = volume(pth, ns, name, pvName, nodeName, caps, false)
	}

	out := make([]Volume, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

func volume(path, ns, name, pvName, node string, caps map[string]int64, fromPV bool) Volume {
	vol := Volume{
		Path:      path,
		Namespace: ns,
		Name:      name,
		PV:        pvName,
		Node:      node,
		FromPV:    fromPV,
	}
	if bytes, ok := caps[ns+"/"+name]; ok && bytes > 0 {
		vol.Capacity = bytes
		vol.CapacityFromPVC = true
	}
	return vol
}

func capacities(pvcs []*corev1.PersistentVolumeClaim) map[string]int64 {
	out := make(map[string]int64, len(pvcs))
	for _, pvc := range pvcs {
		if pvc == nil || !validIdent(pvc.Namespace) || !validIdent(pvc.Name) {
			continue
		}
		q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if !ok || q.Value() <= 0 {
			continue
		}
		out[pvc.Namespace+"/"+pvc.Name] = q.Value()
	}
	return out
}

func identity(pv *corev1.PersistentVolume, base string) (pvName, namespace, name string, ok bool) {
	if ref := pv.Spec.ClaimRef; ref != nil && validIdent(ref.Namespace) && validIdent(ref.Name) && validIdent(pv.Name) {
		return pv.Name, ref.Namespace, ref.Name, true
	}
	parsed, ns, pvcName, pok := ParseDirName(base)
	if !pok {
		return "", "", "", false
	}
	if validIdent(pv.Name) {
		return pv.Name, ns, pvcName, true
	}
	return parsed, ns, pvcName, true
}

// pathWithin returns the first local or hostPath path that stays inside storagePath.
// spec.local.path is preferred when both are present and inside the root.
func pathWithin(pv *corev1.PersistentVolume, storage string) string {
	if pv == nil {
		return ""
	}
	var candidates []string
	if pv.Spec.Local != nil {
		candidates = append(candidates, pv.Spec.Local.Path)
	}
	if pv.Spec.HostPath != nil {
		candidates = append(candidates, pv.Spec.HostPath.Path)
	}
	for _, c := range candidates {
		if !filepath.IsAbs(c) {
			continue
		}
		clean := filepath.Clean(c)
		rel, err := filepath.Rel(storage, clean)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		return clean
	}
	return ""
}

func termMatches(term corev1.NodeSelectorTerm, nodeName string) bool {
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	for _, exp := range term.MatchExpressions {
		if !requirementMatches(exp.Key, exp.Operator, exp.Values, nodeName) {
			return false
		}
	}
	for _, exp := range term.MatchFields {
		if !requirementMatches(exp.Key, exp.Operator, exp.Values, nodeName) {
			return false
		}
	}
	return true
}

func requirementMatches(key string, op corev1.NodeSelectorOperator, values []string, nodeName string) bool {
	known := key == corev1.LabelHostname || key == "metadata.name"
	switch op {
	case corev1.NodeSelectorOpIn:
		if !contains(values, nodeName) {
			return false
		}
		if known {
			return true
		}
		// local-path writes a single node name into a configurable affinity key.
		return len(values) == 1 && values[0] == nodeName
	case corev1.NodeSelectorOpNotIn:
		if !known {
			return false
		}
		return !contains(values, nodeName)
	default:
		// Exists / Gt / Lt need labels we do not have. Do not assume a match.
		return false
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func cutLast(s, sep string) (head, tail string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i <= 0 || i >= len(s)-len(sep) {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

func validIdent(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 253 {
		return false
	}
	return !strings.ContainsAny(s, "/\\\n\"{}")
}
