package types

import "testing"

func TestParseGPUSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    GPUSpec
		wantErr bool
	}{
		{"", GPUSpec{AnyModel: true, Count: 1}, false},
		{"any", GPUSpec{AnyModel: true, Count: 1}, false},
		{"ANY", GPUSpec{AnyModel: true, Count: 1}, false},
		{"  any  ", GPUSpec{AnyModel: true, Count: 1}, false},
		{"count:2", GPUSpec{AnyModel: true, Count: 2}, false},
		{"count:1", GPUSpec{AnyModel: true, Count: 1}, false},
		{"4090", GPUSpec{Model: "4090", Count: 1}, false},
		{"a6000", GPUSpec{Model: "a6000", Count: 1}, false},
		{"count:0", GPUSpec{}, true},
		{"count:-3", GPUSpec{}, true},
		{"count:", GPUSpec{}, true},
		{"count", GPUSpec{}, true},
	}
	for _, tt := range tests {
		got, err := ParseGPUSpec(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseGPUSpec(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("ParseGPUSpec(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestGPUSpecMatches(t *testing.T) {
	g := GPU{Name: "NVIDIA GeForce RTX 4090"}
	cases := []struct {
		spec GPUSpec
		want bool
	}{
		{GPUSpec{AnyModel: true, Count: 1}, true},
		{GPUSpec{Model: "4090", Count: 1}, true},
		{GPUSpec{Model: "RTX", Count: 1}, true},
		{GPUSpec{Model: "rtx", Count: 1}, true}, // case-insensitive
		{GPUSpec{Model: "a6000", Count: 1}, false},
	}
	for _, c := range cases {
		if got := c.spec.Matches(g); got != c.want {
			t.Errorf("%+v.Matches(%q) = %v, want %v", c.spec, g.Name, got, c.want)
		}
	}
}

func TestJobStateIsTerminal(t *testing.T) {
	terminal := []JobState{JobSucceeded, JobFailed, JobKilled}
	nonTerminal := []JobState{JobQueued, JobAssigned, JobRunning}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestGPUGlobalID(t *testing.T) {
	cases := []struct {
		gpu  GPU
		want string
	}{
		{GPU{NodeID: "office-a", Index: 0}, "office-a:0"},
		{GPU{NodeID: "rig-home", Index: 3}, "rig-home:3"},
	}
	for _, c := range cases {
		if got := c.gpu.GlobalID(); got != c.want {
			t.Errorf("GlobalID() = %q, want %q", got, c.want)
		}
	}
}

func TestNodeIsProvider(t *testing.T) {
	provider := Node{Role: RoleProvider, GPUs: []GPU{{Index: 0}}}
	if !provider.IsProvider() {
		t.Error("provider with a GPU should be a provider")
	}
	// A node tagged provider but reporting zero GPUs is not a usable provider.
	emptyProvider := Node{Role: RoleProvider}
	if emptyProvider.IsProvider() {
		t.Error("provider with no GPUs should not count as a provider")
	}
	client := Node{Role: RoleClient}
	if client.IsProvider() {
		t.Error("client should not be a provider")
	}
}
