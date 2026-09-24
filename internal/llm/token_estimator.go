package llm

import (
	"math"
	"unicode/utf8"
)

// No tokenizer for this gateway is available. Estimate Latin/code at 3 ASCII
// characters/token and non-ASCII at 2 UTF-8 bytes/token, plus a 20% margin.
// These are heuristics, not model capacity guarantees. Real prompt usage can
// raise the factor; never use completion tokens or cumulative usage to calibrate.
func estimatedTextTokens(text string) int {
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII += utf8.RuneLen(r)
		}
	}
	return (ascii+2)/3 + (nonASCII+1)/2
}

// One estimator per Diagnose, shared across tool binding and contract retry.
// It never crosses a model switch and never stores prompt contents.
type tokenEstimator struct {
	factor float64
}

func (e *tokenEstimator) scale() float64 {
	if e == nil || e.factor < 1.2 {
		return 1.2
	}
	return e.factor
}

func (e *tokenEstimator) estimate(raw int) int {
	return int(math.Ceil(float64(raw) * e.scale()))
}

func (e *tokenEstimator) observe(raw, promptTokens int) {
	if e == nil || raw <= 0 || promptTokens <= 0 {
		return
	}
	// Keep the largest observed underestimate with 10% headroom, so a short
	// cheap response cannot erase the safety margin learned from dense text.
	e.factor = math.Max(e.scale(), float64(promptTokens)/float64(raw)*1.1)
}
