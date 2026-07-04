package machine

import "sort"

// KeyEstimate is one destructured scope value and its estimated token size.
type KeyEstimate struct {
	Key    string
	Tokens int
}

// LargestInputs attributes a state's rendered input to the scope values its
// prompt and system functions destructure, largest first — an input-budget
// overflow can name its offenders instead of just its total. Best-effort:
// only destructuring params can be attributed (`s => ...` cannot), values
// render exactly as the distiller would see them, and the walk runs only on
// the overflow path, never per call.
func LargestInputs(st *State, scope map[string]any) []KeyEstimate {
	a := st.Agent
	if a == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []KeyEstimate
	for _, d := range []Dyn{a.Prompt, a.System} {
		if !d.IsFn() || d.Src == "" {
			continue
		}
		keys, ok := destructuredKeys(d.Src)
		if !ok {
			continue
		}
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			v, present := scope[k]
			if !present || v == nil {
				continue
			}
			text, err := RenderDistillSource(v)
			if err != nil {
				continue
			}
			out = append(out, KeyEstimate{Key: k, Tokens: EstimateTokens(text)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
	return out
}
