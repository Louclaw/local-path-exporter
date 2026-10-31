package priority

import "testing"

func TestLowerDoesNotPanic(t *testing.T) {
	Lower()
	Lower()
}
