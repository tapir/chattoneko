package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"chattoneko/internal/mcphub"
)

// EnvLocationString is the environment variable that optionally supplies a
// free-form location string (e.g. "Berlin, Germany"). When set (non-empty
// after trimming), the time_location tool appends it to its result so the
// model can ground place-aware answers; when unset, nothing is appended.
// Read at call time, like the clock itself.
const EnvLocationString = "CHATTO_LOCATION_STRING"

// The "time_location" tool: returns the server's current local date, time,
// and timezone — plus its configured location when CHATTO_LOCATION_STRING is
// set — so the model can ground relative expressions ("tomorrow", "next
// Friday", "in two hours") and place-aware answers ("near me", "local").
//
// All user/LLM-facing text is hardcoded here — edit in place to change it.
var TimeLocation = Tool{
	Name: "time_location",
	Description: "Get the current date, time, timezone, and — when the " +
		"server has one configured — location. Call this when the answer " +
		"depends on what day or time it is now, or on where the user is.",
	// No arguments.
	Schema:         nil, // defaults to an empty object schema (see New)
	DefaultEnabled: true,
	Handler:        timeLocation,
}

// timeLayout is the human-readable part of the result; the RFC 3339
// timestamp appended after it keeps the answer machine-unambiguous.
const timeLayout = "Monday, 2 January 2006, 15:04:05 MST"

func timeLocation(_ context.Context, _ string, _ mcphub.CallMeta) (string, error) {
	now := time.Now()
	out := fmt.Sprintf("%s (%s)", now.Format(timeLayout), now.Format(time.RFC3339))
	if loc := strings.TrimSpace(os.Getenv(EnvLocationString)); loc != "" {
		out += " — " + loc
	}
	return out, nil
}
