package inventory

import (
	"context"
	"errors"
	"testing"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []types.GPU
		wantErr bool
	}{
		{
			name: "multi-GPU",
			raw: "0, NVIDIA GeForce RTX 4090, 24564, 1234, 15\n" +
				"1, NVIDIA RTX A6000, 49140, 0, 0\n",
			want: []types.GPU{
				{NodeID: "office-a", Index: 0, Name: "NVIDIA GeForce RTX 4090", MemTotalMB: 24564, MemUsedMB: 1234, UtilPercent: 15},
				{NodeID: "office-a", Index: 1, Name: "NVIDIA RTX A6000", MemTotalMB: 49140, MemUsedMB: 0, UtilPercent: 0},
			},
		},
		{
			name: "single GPU",
			raw:  "0, NVIDIA GeForce RTX 3090, 24268, 500, 42\n",
			want: []types.GPU{
				{NodeID: "office-a", Index: 0, Name: "NVIDIA GeForce RTX 3090", MemTotalMB: 24268, MemUsedMB: 500, UtilPercent: 42},
			},
		},
		{
			name: "zero GPUs (empty output)",
			raw:  "",
			want: []types.GPU{},
		},
		{
			name: "zero GPUs (no devices message)",
			raw:  "No devices were found",
			want: []types.GPU{},
		},
		{
			name: "trailing blank lines tolerated",
			raw:  "0, Tesla T4, 15360, 100, 3\n\n",
			want: []types.GPU{
				{NodeID: "office-a", Index: 0, Name: "Tesla T4", MemTotalMB: 15360, MemUsedMB: 100, UtilPercent: 3},
			},
		},
		{
			name: "N/A field coerced to zero",
			raw:  "0, NVIDIA A100, 40960, [N/A], [N/A]\n",
			want: []types.GPU{
				{NodeID: "office-a", Index: 0, Name: "NVIDIA A100", MemTotalMB: 40960, MemUsedMB: 0, UtilPercent: 0},
			},
		},
		{
			name:    "malformed: too few fields",
			raw:     "0, NVIDIA GeForce RTX 4090, 24564\n",
			wantErr: true,
		},
		{
			name:    "malformed: non-integer memory",
			raw:     "0, NVIDIA GeForce RTX 4090, twentyfour, 1234, 15\n",
			wantErr: true,
		},
		{
			name:    "malformed: non-integer index",
			raw:     "x, NVIDIA GeForce RTX 4090, 24564, 1234, 15\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse("office-a", []byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse() err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Parse() returned %d GPUs, want %d (%+v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("GPU[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDetectInjectsRunner(t *testing.T) {
	d := Detector{run: func(ctx context.Context) ([]byte, error) {
		return []byte("0, NVIDIA GeForce RTX 4090, 24564, 1234, 15\n"), nil
	}}
	gpus, err := d.Detect(context.Background(), "rig-home")
	if err != nil {
		t.Fatalf("Detect() error: %v", err)
	}
	if len(gpus) != 1 || gpus[0].NodeID != "rig-home" || gpus[0].Index != 0 {
		t.Fatalf("Detect() = %+v, want 1 GPU on rig-home", gpus)
	}
}

func TestDetectPropagatesNoNvidiaSmi(t *testing.T) {
	d := Detector{run: func(ctx context.Context) ([]byte, error) {
		return nil, ErrNoNvidiaSmi
	}}
	_, err := d.Detect(context.Background(), "laptop")
	if !errors.Is(err, ErrNoNvidiaSmi) {
		t.Fatalf("Detect() err=%v, want ErrNoNvidiaSmi (so agent runs as client)", err)
	}
}
