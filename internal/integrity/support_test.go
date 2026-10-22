package integrity_test

import (
	"testing"

	"github.com/inode64/fsledger/internal/integrity"
)

func TestAttributesSupportedClassifiesStatuses(t *testing.T) {
	t.Parallel()

	if !integrity.AttributesSupported("") || !integrity.AttributesSupported(integrity.AttributesAvailable) ||
		!integrity.AttributesSupported(integrity.AttributesTooLarge) ||
		integrity.AttributesSupported(integrity.AttributesUnsupported) {
		t.Fatal("AttributesSupported misclassifies statuses")
	}
}
