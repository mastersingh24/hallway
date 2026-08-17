// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command report regenerates a company-filtered markdown report: every session
// with at least one speaker from the target company — listing *all* speakers on
// those sessions, not just the matching ones — plus a roster of that company's
// speakers.
//
// Usage:
//
//	go run ./cmd/report                                   # from local JSON files
//	go run ./cmd/report -refresh                          # refresh from the API first
//	go run ./cmd/report -event kubecon-eu-2027 -refresh   # a different conference
//	go run ./cmd/report -company Microsoft -out ms.md
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gke-demos/hallway/core"
)

func main() {
	var (
		allPath = flag.String("all", "all.json", "path to Sessionize /All export")
		spkPath = flag.String("speakers", "speakers.json", "path to Sessionize /Speakers export")
		outPath = flag.String("out", "", "markdown file to write (default: <event>-<company>-<date>.md)")
		company = flag.String("company", "Google", "company to filter on (case-insensitive substring)")

		eventSlug = flag.String("event", core.DefaultEventSlug,
			"conference to report on; known: "+strings.Join(core.EventSlugs(), ", "))
		refresh = flag.Bool("refresh", false,
			"refresh the local JSON files from the Sessionize API before generating")
		code = flag.String("sessionize-code", "",
			"override the event's Sessionize API code (implies -refresh)")
		base = flag.String("sessionize-url", core.DefaultSessionizeBase,
			"Sessionize API base URL (used only when refreshing)")
	)
	flag.Parse()

	ctx := context.Background()

	event, err := core.LookupEvent(*eventSlug)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *code != "" {
		event.SessionizeCode = *code
	}

	if *refresh || *code != "" {
		f := core.NewFetcher(*base, event.SessionizeCode)
		log.Printf("refreshing from Sessionize (%s, code=%s)", f.BaseURL, f.Code)
		if err := f.Refresh(ctx, *allPath, *spkPath); err != nil {
			log.Fatalf("refresh failed: %v", err)
		}
		log.Printf("refreshed %s and %s", *allPath, *spkPath)
	}

	idx, err := core.Load(event, *allPath, *spkPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The data files aren't in version control, so this is what a fresh
			// clone hits first. Say how to fix it rather than just what failed.
			log.Fatalf("load data: %v\n\nNo local data yet — run with -refresh to download it from Sessionize:\n\n\tgo run ./cmd/report -refresh\n", err)
		}
		log.Fatalf("load data: %v", err)
	}
	log.Printf("loaded %d sessions, %d speakers", len(idx.SessionList), len(idx.SpeakerList))

	sessions := idx.FindSessions(core.SessionFilter{Company: *company})

	// Roster: speakers at the target company (index is already name-sorted).
	needle := strings.ToLower(*company)
	var roster []string
	for _, sp := range idx.SpeakerList {
		if strings.Contains(strings.ToLower(sp.Company), needle) {
			roster = append(roster, sp.Name)
		}
	}

	tz := event.TZShort

	// The schedule shifts daily right up to the event, so every report is
	// stamped with the day it was generated — in the title and the filename, so
	// two reports for the same conference can't be confused for each other.
	generated := time.Now().Format("2006-01-02")

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s Sessions (as of %s)\n\n", event.Name, *company, generated)
	fmt.Fprintf(&b, "_%s · %s. Times in %s. Source: Sessionize API (`%s`). Generated %s._\n\n",
		event.Location, event.Dates, event.TZLabel, event.SessionizeCode, generated)

	fmt.Fprintf(&b, "## Sessions with at least one %s speaker (%d)\n\n", *company, len(sessions))
	b.WriteString("| Session | Date/Time (" + tz + ") | Speakers |\n|---|---|---|\n")
	for _, s := range sessions {
		names := make([]string, 0, len(s.Speakers))
		for _, sp := range s.Speakers {
			if sp.Company != "" {
				names = append(names, fmt.Sprintf("%s (%s)", sp.Name, sp.Company))
			} else {
				names = append(names, sp.Name)
			}
		}
		fmt.Fprintf(&b, "| [%s](%s) | %s | %s |\n",
			escapePipes(s.Title), s.Link, timeRange(s.StartsAt, s.EndsAt, tz), escapePipes(strings.Join(names, "; ")))
	}

	fmt.Fprintf(&b, "\n## %s speakers (%d)\n\n", *company, len(roster))
	b.WriteString("| Speaker |\n|---|\n")
	for _, n := range roster {
		fmt.Fprintf(&b, "| %s |\n", escapePipes(n))
	}

	out := *outPath
	if out == "" {
		out = fmt.Sprintf("%s-%s-%s.md", event.Slug, slugify(*company), generated)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	log.Printf("wrote %s — %d sessions, %d %s speakers", out, len(sessions), len(roster), *company)
}

// slugify makes a company name safe for a filename: "Red Hat" -> "red-hat".
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// timeRange renders "Tue Nov 10, 2026 · 11:30 AM–12:50 PM MST" from the
// as-published local timestamps (Sessionize emits them without an offset).
func timeRange(startsAt, endsAt, tz string) string {
	const layout = "2006-01-02T15:04:05"
	st, err1 := time.Parse(layout, startsAt)
	et, err2 := time.Parse(layout, endsAt)
	if err1 != nil || err2 != nil {
		return startsAt + "–" + endsAt
	}
	return fmt.Sprintf("%s · %s–%s %s",
		st.Format("Mon Jan 2, 2006"), st.Format("3:04 PM"), et.Format("3:04 PM"), tz)
}

// escapePipes keeps stray pipes in titles/names from breaking table cells.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
