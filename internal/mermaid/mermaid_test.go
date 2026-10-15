package mermaid

import (
	"strings"
	"testing"
)

func TestRenderGolden(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{
			"simple LR chain with labels",
			`graph LR
  A[Client] -->|request| B[Server] -->|query| C[(Database)]`,
			`┌────────┐               ┌────────┐             ╭──────────╮
│ Client ├─── request ──►│ Server ├─── query ──►│ Database │
└────────┘               └────────┘             ╰──────────╯`,
		},
		{
			"TD decision with merging branches",
			`flowchart TD
  A[Start] --> B{Is it valid?}
  B -->|Yes| C[Process]
  B -->|No| D[Reject]
  C --> E[Done]
  D --> E`,
			`        ┌───────┐
        │ Start │
        └───┬───┘
            │
            ▼
    ╱──────────────╲
    │ Is it valid? │
    ╲──────┬───────╱
     ╭─Yes─┴─ No ─╮
     │            │
     ▼            ▼
┌─────────┐   ┌────────┐
│ Process │   │ Reject │
└────┬────┘   └───┬────┘
     ╰─────┬──────╯
           │
           ▼
        ┌──────┐
        │ Done │
        └──────┘`,
		},
		{
			"subgraph with edges in and out",
			`flowchart TD
  In[Request] --> X
  subgraph S[Service]
    X[Parse] --> Y[Handle]
  end
  Y --> Out[Response]`,
			`  ┌─────────┐
  │ Request │
  └────┬────┘
       │
       │
┌──────┼─────┐
│ Service    │
│      ▼     │
│  ┌───────┐ │
│  │ Parse │ │
│  └───┬───┘ │
│      │     │
│      ▼     │
│ ┌────────┐ │
│ │ Handle │ │
│ └───┬────┘ │
│     │      │
│     │      │
└─────┼──────┘
      │
      ▼
 ┌──────────┐
 │ Response │
 └──────────┘`,
		},
		{
			"cycle",
			`graph LR
  A --> B --> C --> A`,
			`  ╭───────────────────╮
  ▼                   │
┌───┐     ┌───┐     ┌─┴─┐
│ A ├────►│ B ├────►│ C │
└───┘     └───┘     └───┘`,
		},
		{
			"multi-line labels",
			`graph TD
  A["First line<br>second (line)"] --> B(Rounded<br/>two)
  A --> C([Stadium])`,
			`    ┌───────────────┐
    │  First line   │
    │ second (line) │
    └───────┬───────┘
     ╭──────┴──────╮
     │             │
     ▼             ▼
╭─────────╮   ╭─────────╮
│ Rounded │   │ Stadium │
│   two   │   ╰─────────╯
╰─────────╯`,
		},
		{
			"edge to a subgraph, dotted text label",
			`flowchart LR
  Cloud --> VSC
  subgraph VSC[VS Code (Lokal)]
    E[Edit Script Luau]
    P[default.project.json]
  end
  E -->|Rojo Sync| S[Studio]
  S -->|Save Cmd+S| D[KeepSilent.rbxl di Disk]
  Cloud -. rbxassetid:// ... .-> S`,
			`    ╭┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄ rbxassetid:// ... ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄╮
    ┆                                                            ┆
    ┆         ┌─ VS Code (Lokal) ──────────┐                     ┆
    ┆         │                            │                     ▼
    ┆         │    ┌──────────────────┐    │                 ┌────────┐                  ┌─────────────────────────┐
    ┆         │    │ Edit Script Luau ├────┼── Rojo Sync ───►│ Studio ├─── Save Cmd+S ──►│ KeepSilent.rbxl di Disk │
┌───┴───┐     │    └──────────────────┘    │                 └────────┘                  └─────────────────────────┘
│ Cloud ├────►│                            │
└───────┘     │  ┌──────────────────────┐  │
              │  │ default.project.json │  │
              │  └──────────────────────┘  │
              │                            │
              └────────────────────────────┘`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got:\n%s\n\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRenderUnsupported(t *testing.T) {
	for _, src := range []string{"sequenceDiagram\n  A->>B: hi", "classDiagram\n  A <|-- B", "pie\n  \"a\": 1"} {
		if out, err := Render(src, 80); err == nil {
			t.Errorf("%q: expected error, got:\n%s", strings.SplitN(src, "\n", 2)[0], out)
		}
	}
}
func TestRenderFallsBackToTopDown(t *testing.T) {
	src := "graph LR\n  A[First step] --> B[Second step] --> C[Third step] --> D[Fourth step]"
	wide, _ := Render(src, 0)
	got, err := Render(src, 40)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "\n") <= strings.Count(wide, "\n") {
		t.Fatalf("expected a top-down layout, got:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if w := strWidth(l); w > 40 {
			t.Fatalf("line wider than 40 (%d):\n%s", w, got)
		}
	}
}
