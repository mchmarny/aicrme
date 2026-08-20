package steps

import (
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/prove"
)

// White-box: NewProve returns engine.Step, which gives a black-box test
// nothing to inspect without actually waiting out whatever GangTimeout was
// applied. This checks the defaulted value directly instead.
func TestNewProveDefaultsGangTimeout(t *testing.T) {
	s := NewProve(prove.NewClient(fake.NewSimpleClientset()), ProveConfig{}).(*proveStep)
	if s.cfg.GangTimeout != defaultGangTimeout {
		t.Errorf("GangTimeout = %s, want the default %s", s.cfg.GangTimeout, defaultGangTimeout)
	}
}

func TestNewProveKeepsExplicitGangTimeout(t *testing.T) {
	s := NewProve(prove.NewClient(fake.NewSimpleClientset()), ProveConfig{GangTimeout: 90 * time.Second}).(*proveStep)
	if s.cfg.GangTimeout != 90*time.Second {
		t.Errorf("GangTimeout = %s, want 90s", s.cfg.GangTimeout)
	}
}
