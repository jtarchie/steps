package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

func layoutDump(graph graphView) string {
	var out strings.Builder

	fmt.Fprintf(&out, "%dx%d\n", graph.W, graph.H)

	for _, n := range graph.Nodes {
		fmt.Fprintf(&out, "%s L%d %d,%d %dx%d ring %d,%d %dx%d text %d,%d,%d\n",
			n.ID, n.Layer, n.X, n.Y, n.W, n.H, n.RingX, n.RingY, n.RingW, n.RingH, n.TextX, n.NameY, n.StatusY)
	}

	for _, e := range graph.Edges {
		fmt.Fprintf(&out, "%s>%s %s\n", e.From, e.To, e.Path)
	}

	return out.String()
}

// TestGraphLayoutIsPinned: every other graph test asks a relation (left of, taller than), and a node drawn 6px off its edge or a sweep that stopped reordering breaks none of them.
func TestGraphLayoutIsPinned(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		views   []jobView
		failing map[string]store.CheckError
		want    string
	}{
		{
			name: "release",
			want: `922x181
in:unit:src L0 19,17 45x26 ring 0,0 0x0 text 31,34,0
in:lint:docs L0 8,63 67x26 ring 0,0 0x0 text 20,80,0
job:unit L1 123,8 133x44 ring 120,5 139x50 text 135,26,42
job:lint L1 123,66 133x44 ring 120,63 139x50 text 135,84,100
via:unit:src L2 319,17 45x26 ring 0,0 0x0 text 331,34,0
via:lint:src L2 319,64 45x26 ring 0,0 0x0 text 331,81,0
gap:8:1 L2 315,104 53x26 ring 0,0 0x0 text 327,121,0
in:build:version L2 304,144 75x26 ring 0,0 0x0 text 316,161,0
job:build L3 427,69 191x44 ring 424,66 197x50 text 439,87,103
gap:8:2 L3 496,127 53x26 ring 0,0 0x0 text 508,144,0
out:build:image L4 669,67 60x26 ring 0,0 0x0 text 681,84,0
via:build:src L4 677,107 45x26 ring 0,0 0x0 text 689,124,0
via:lint:docs L4 666,147 67x26 ring 0,0 0x0 text 678,164,0
job:deploy L5 781,89 133x62 ring 778,86 139x68 text 793,107,123
job:build>out:build:image M618,80 C643,80 643,80 669,80
job:unit>via:unit:src M256,30 C287,30 287,30 319,30
via:unit:src>job:build M364,30 C395,30 395,80 427,80
job:lint>via:lint:src M256,77 C287,77 287,77 319,77
via:lint:src>job:build M364,77 C395,77 395,80 427,80
out:build:image>job:deploy M729,80 C755,80 755,99 781,99
job:build>via:build:src M618,102 C647,102 647,120 677,120
via:build:src>job:deploy M722,120 C751,120 751,120 781,120
via:lint:docs>job:deploy M733,160 C757,160 757,140 781,140
in:unit:src>job:unit M64,30 C93,30 93,30 123,30
in:lint:docs>job:lint M75,76 C99,76 99,99 123,99
in:build:version>job:build M379,157 C403,157 403,102 427,102
in:unit:src>job:lint M64,30 C93,30 93,77 123,77
job:lint>gap:8:1 M256,99 C285,99 285,117 315,117
gap:8:1>gap:8:2 M368,117 C432,117 432,140 496,140
gap:8:2>via:lint:docs M549,140 C607,140 607,160 666,160
`,
			views: []jobView{
				{Name: "unit", Inputs: []config.JobInput{trigger("src")}},
				{Name: "lint", Inputs: []config.JobInput{trigger("src"), reads("docs")}},
				{
					Name: "build", Inputs: []config.JobInput{trigger("src", "unit", "lint"), reads("version")},
					Outputs: []string{"image"},
					HasRun:  true, Latest: store.RunRow{Status: "succeeded"}, Queued: &queuedJob{WaitingOn: "src"},
				},
				{Name: "deploy", Inputs: []config.JobInput{reads("image", "build"), reads("src", "build"), trigger("docs", "lint")}},
			},
			failing: map[string]store.CheckError{"docs": {Name: "docs", Message: "nope"}},
		},
		{
			name: "crossed",
			want: `506x149
in:q:b L0 8,17 40x26 ring 0,0 0x0 text 20,34,0
in:p:a L0 8,75 40x26 ring 0,0 0x0 text 20,92,0
job:q L1 96,8 133x44 ring 93,5 139x50 text 108,26,42
job:p L1 96,66 133x44 ring 93,63 139x50 text 108,84,100
out:q:qb L2 277,17 40x26 ring 0,0 0x0 text 289,34,0
out:p:pa L2 277,75 40x26 ring 0,0 0x0 text 289,92,0
in:y:c L2 277,115 40x26 ring 0,0 0x0 text 289,132,0
job:x L3 365,8 133x44 ring 362,5 139x50 text 377,26,42
job:y L3 365,86 133x44 ring 362,83 139x50 text 377,104,120
job:p>out:p:pa M229,88 C253,88 253,88 277,88
job:q>out:q:qb M229,30 C253,30 253,30 277,30
out:q:qb>job:x M317,30 C341,30 341,30 365,30
out:p:pa>job:y M317,88 C341,88 341,97 365,97
in:p:a>job:p M48,88 C72,88 72,88 96,88
in:q:b>job:q M48,30 C72,30 72,30 96,30
in:y:c>job:y M317,128 C341,128 341,119 365,119
`,
			views: []jobView{
				{Name: "p", Inputs: []config.JobInput{trigger("a")}, Outputs: []string{"pa"}},
				{Name: "q", Inputs: []config.JobInput{trigger("b")}, Outputs: []string{"qb"}},
				{Name: "x", Inputs: []config.JobInput{trigger("qb", "q")}},
				{Name: "y", Inputs: []config.JobInput{trigger("pa", "p"), trigger("c")}},
			},
		},
		{
			name: "unrelated",
			want: `330x109
job:publish L0 8,8 133x44 ring 5,5 139x50 text 20,26,42
in:check:docs L0 48,66 53x26 ring 0,0 0x0 text 60,83,0
out:publish:site L1 229,17 53x26 ring 0,0 0x0 text 241,34,0
job:check L1 189,57 133x44 ring 186,54 139x50 text 201,75,91
job:publish>out:publish:site M141,30 C185,30 185,30 229,30
in:check:docs>job:check M101,79 C145,79 145,79 189,79
`,
			views: []jobView{
				{Name: "publish", Outputs: []string{"site"}},
				{Name: "check", Inputs: []config.JobInput{reads("docs")}},
			},
		},
		{
			name: "idle beside a builder",
			want: `257x147
job:idle L0 8,8 133x44 ring 5,5 139x50 text 20,26,42
job:build L0 8,75 133x44 ring 5,72 139x50 text 20,93,109
out:build:image L1 189,73 60x26 ring 0,0 0x0 text 201,90,0
out:build:chart L1 189,113 60x26 ring 0,0 0x0 text 201,130,0
job:build>out:build:image M141,86 C165,86 165,86 189,86
job:build>out:build:chart M141,108 C165,108 165,126 189,126
`,
			views: []jobView{
				{Name: "idle"},
				{Name: "build", Outputs: []string{"image", "chart"}},
			},
		},
		{
			name: "source job",
			want: `639x122
in:compile:src L0 8,17 45x26 ring 0,0 0x0 text 20,34,0
job:compile L1 101,8 133x44 ring 98,5 139x50 text 113,26,42
out:compile:bin L2 282,8 45x26 ring 0,0 0x0 text 294,25,0
via:compile:src L2 282,48 45x26 ring 0,0 0x0 text 294,65,0
in:ship:key L2 282,88 45x26 ring 0,0 0x0 text 294,105,0
job:ship L3 375,30 133x62 ring 372,27 139x68 text 387,48,64
out:ship:release L4 556,48 75x26 ring 0,0 0x0 text 568,65,0
job:compile>out:compile:bin M234,19 C258,19 258,21 282,21
job:ship>out:ship:release M508,61 C532,61 532,61 556,61
out:compile:bin>job:ship M327,21 C351,21 351,40 375,40
job:compile>via:compile:src M234,41 C258,41 258,61 282,61
via:compile:src>job:ship M327,61 C351,61 351,61 375,61
in:compile:src>job:compile M53,30 C77,30 77,30 101,30
in:ship:key>job:ship M327,101 C351,101 351,81 375,81
`,
			views: []jobView{
				{Name: "compile", Inputs: []config.JobInput{reads("src")}, Outputs: []string{"bin"}},
				{
					Name:    "ship",
					Inputs:  []config.JobInput{trigger("bin", "compile"), trigger("key"), trigger("src", "compile")},
					Outputs: []string{"release"},
				},
			},
		},
		{
			name: "converging",
			want: `511x395
job:j0 L0 8,8 133x44 ring 5,5 139x50 text 20,26,42
in:j3:r4 L0 54,66 40x26 ring 0,0 0x0 text 66,83,0
job:j1 L0 8,164 133x62 ring 5,161 139x68 text 20,182,198
job:j4 L0 8,286 133x44 ring 5,283 139x50 text 20,304,320
via:j0:r2 L1 235,17 40x26 ring 0,0 0x0 text 247,34,0
job:j3 L1 189,57 133x44 ring 186,54 139x50 text 201,75,91
via:j1:r1 L1 235,161 40x26 ring 0,0 0x0 text 247,178,0
in:j2:r4 L1 235,201 40x26 ring 0,0 0x0 text 247,218,0
out:j1:r4 L1 235,241 40x26 ring 0,0 0x0 text 247,258,0
out:j1:r0 L1 235,281 40x26 ring 0,0 0x0 text 247,298,0
out:j4:r0 L1 235,321 40x26 ring 0,0 0x0 text 247,338,0
out:j4:r1 L1 235,361 40x26 ring 0,0 0x0 text 247,378,0
out:j3:r0 L2 416,66 40x26 ring 0,0 0x0 text 428,83,0
job:j2 L2 370,108 133x62 ring 367,105 139x68 text 382,126,142
job:j1>out:j1:r4 M141,195 C188,195 188,254 235,254
job:j1>out:j1:r0 M141,215 C188,215 188,294 235,294
job:j3>out:j3:r0 M322,79 C369,79 369,79 416,79
job:j4>out:j4:r0 M141,297 C188,297 188,334 235,334
job:j4>out:j4:r1 M141,319 C188,319 188,374 235,374
job:j0>via:j0:r2 M141,30 C188,30 188,30 235,30
via:j0:r2>job:j2 M275,30 C322,30 322,118 370,118
job:j1>via:j1:r1 M141,174 C188,174 188,174 235,174
via:j1:r1>job:j2 M275,174 C322,174 322,139 370,139
in:j2:r4>job:j2 M275,214 C322,214 322,159 370,159
in:j3:r4>job:j3 M94,79 C141,79 141,79 189,79
`,
			views: []jobView{
				{Name: "j0"},
				{Name: "j1", Outputs: []string{"r4", "r0"}},
				{Name: "j2", Inputs: []config.JobInput{trigger("r2", "j0"), reads("r4"), reads("r1", "j1")}},
				{Name: "j3", Inputs: []config.JobInput{reads("r4")}, Outputs: []string{"r0", "r0"}},
				{Name: "j4", Outputs: []string{"r0", "r1"}},
			},
		},
		{
			name: "cycle",
			want: `506x60
via:a:r L0 8,8 40x26 ring 0,0 0x0 text 20,25,0
job:b L1 96,8 133x44 ring 93,5 139x50 text 108,26,42
via:b:r L2 277,17 40x26 ring 0,0 0x0 text 289,34,0
job:a L3 365,8 133x44 ring 362,5 139x50 text 377,26,42
job:b>via:b:r M229,30 C253,30 253,30 277,30
via:b:r>job:a M317,30 C341,30 341,30 365,30
job:a>via:a:r M498,30 C253,30 253,21 8,21
via:a:r>job:b M48,21 C72,21 72,30 96,30
`,
			views: []jobView{
				{Name: "a", Inputs: []config.JobInput{trigger("r", "b")}},
				{Name: "b", Inputs: []config.JobInput{trigger("r", "a")}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := layoutDump(buildGraph(tc.views, tc.failing)); got != tc.want {
				t.Errorf("layout changed; if the change is intended, this is the new want:\n%s", got)
			}
		})
	}
}
