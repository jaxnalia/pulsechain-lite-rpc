package config

import (
	"strings"
	"testing"
)

func TestDefaultModeIsRPC(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeRPC || cfg.Mode.Executes() {
		t.Fatalf("default mode %q, want rpc without execution", cfg.Mode)
	}
}

func TestSizeDefaultsFollowMode(t *testing.T) {
	rpc, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rpc.CacheMaxBytes != 1<<30 || rpc.DiskLimit != 3<<30 || rpc.DiskTotalLimit != 8e9 {
		t.Fatalf("rpc sizes: cache=%d exec=%d total=%d", rpc.CacheMaxBytes, rpc.DiskLimit, rpc.DiskTotalLimit)
	}
	full, err := Parse([]string{"--mode", "full", "--experimental.validator"})
	if err != nil {
		t.Fatal(err)
	}
	if full.CacheMaxBytes != 4<<30 || full.DiskLimit != 10<<30 || full.DiskTotalLimit != 25e9 {
		t.Fatalf("full sizes: cache=%d exec=%d total=%d", full.CacheMaxBytes, full.DiskLimit, full.DiskTotalLimit)
	}
	t.Setenv("PULSELITE_CACHE_MAX_BYTES", "") // empty env (compose pass-through) = default
	over, err := Parse([]string{"--cache.max-bytes", "512MiB"})
	if err != nil {
		t.Fatal(err)
	}
	if over.CacheMaxBytes != 512<<20 {
		t.Fatalf("explicit cache size ignored: %d", over.CacheMaxBytes)
	}
}

func TestValidatingModesNeedExperimentalFlag(t *testing.T) {
	for _, mode := range []string{"validator", "full"} {
		if _, err := Parse([]string{"--mode", mode}); err == nil || !strings.Contains(err.Error(), "experimental") {
			t.Fatalf("mode %s without flag: err = %v, want experimental hint", mode, err)
		}
		cfg, err := Parse([]string{"--mode", mode, "--experimental.validator"})
		if err != nil {
			t.Fatalf("mode %s with flag: %v", mode, err)
		}
		if !cfg.Mode.Executes() {
			t.Fatalf("mode %s should execute blocks", mode)
		}
	}
}
