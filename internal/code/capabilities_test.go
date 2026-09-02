package code

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestNodePermissionModelProbeRequiresRealEnforcement(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is not installed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := probeNodePermissionModel(ctx, node); err != nil {
		t.Fatalf("Node permission model probe = %v", err)
	}
}
