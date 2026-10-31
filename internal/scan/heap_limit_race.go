//go:build race

package scan

// The race detector inflates the heap. The cap stays far under the ~35MiB the
// old walk retained for this tree, and under the 128Mi pod limit.
const flatTreeHeapLimit = 32 << 20

const flatTreeHeapSlack = 16 << 20
