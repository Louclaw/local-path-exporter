//go:build !race

package scan

// flatTreeHeapLimit is the allowed peak heap growth while scanning 100k files
// and 2k hard links. v0.1.0 used about 35MiB on this tree.
const flatTreeHeapLimit = 8 << 20

// flatTreeHeapSlack is how far the 100k peak may sit above the 2k peak.
const flatTreeHeapSlack = 4 << 20
