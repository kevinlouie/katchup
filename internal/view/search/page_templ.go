// Hand-maintained view (the shipped template is this Go file, not a .templ).
package search

import (
	"context"
	"fmt"
	"html"
	"io"
	"strings"

	"katchup/internal/view/layout"

	"github.com/a-h/templ"
)

// PageData drives the /search results page.
type PageData struct {
	Title     string
	Query     string
	AccountID int64
	Accounts  []AccountView
	Results   []ResultView
	// Backend labels which path served the results ("meilisearch" or "database").
	Backend string
	// Searched is true once a non-empty query has been run (controls empty states).
	Searched bool
}

type AccountView struct {
	ID   int64
	Name string
}

type ResultView struct {
	ID      int64
	Date    string
	From    string
	Subject string
	Folder  string
}

// SearchPage renders the header-only search form and its results.
func SearchPage(data PageData) templ.Component {
	inner := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<div class="rise">`)
		b.WriteString(layout.PageHeader("Archive", "Search", "Full-text search over message headers (subject, from, to). Bodies stay encrypted and are never indexed.", ""))

		inputCls := "rounded-xl border border-white/10 bg-ink-950/60 px-3.5 py-2.5 text-sm text-white outline-none transition focus:border-ketchup/60 focus:ring-2 focus:ring-ketchup/25"

		b.WriteString(`<form method="GET" action="/search" class="mb-6 flex flex-wrap items-end gap-3 rounded-2xl border border-white/10 bg-ink-900/60 p-4">
			<div class="flex flex-1 flex-col gap-1.5" style="min-width:240px">
				<label for="q" class="font-mono text-[11px] uppercase tracking-widest text-mute">Query</label>`)
		fmt.Fprintf(&b, `<input type="search" name="q" id="q" value="%s" placeholder="subject or sender…" class="%s">`, html.EscapeString(data.Query), inputCls)
		b.WriteString(`</div>
			<div class="flex flex-col gap-1.5">
				<label for="account" class="font-mono text-[11px] uppercase tracking-widest text-mute">Account</label>
				<select name="account" id="account" class="` + inputCls + ` min-w-[200px]">
					<option value="">All accounts</option>`)
		for _, acct := range data.Accounts {
			sel := ""
			if acct.ID == data.AccountID {
				sel = " selected"
			}
			fmt.Fprintf(&b, `<option value="%d"%s>%s</option>`, acct.ID, sel, html.EscapeString(acct.Name))
		}
		b.WriteString(`</select>
			</div>
			<button type="submit" class="inline-flex items-center gap-2 rounded-xl bg-ketchup px-4 py-2.5 text-sm font-semibold text-white shadow-lg shadow-ketchup/25 transition hover:bg-ketchup-600">
				<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg>
				Search
			</button>
		</form>`)

		// Empty states.
		if !data.Searched {
			b.WriteString(`<div class="rounded-2xl border border-dashed border-white/12 bg-ink-900/40 px-6 py-20 text-center">
				<div class="mx-auto mb-4 grid h-14 w-14 place-items-center rounded-2xl bg-white/5 text-2xl ring-1 ring-inset ring-white/10">🔍</div>
				<h3 class="font-display text-lg font-semibold text-white">Search your archive</h3>
				<p class="mx-auto mt-1 max-w-sm text-sm text-mute">Enter a subject or sender above. Only headers are searched — message bodies remain encrypted.</p>
			</div></div>`)
			_, err := io.WriteString(w, b.String())
			return err
		}

		if len(data.Results) == 0 {
			fmt.Fprintf(&b, `<div class="rounded-2xl border border-dashed border-white/12 bg-ink-900/40 px-6 py-20 text-center">
				<div class="mx-auto mb-4 grid h-14 w-14 place-items-center rounded-2xl bg-white/5 text-2xl ring-1 ring-inset ring-white/10">📭</div>
				<h3 class="font-display text-lg font-semibold text-white">No matches</h3>
				<p class="mx-auto mt-1 max-w-sm text-sm text-mute">Nothing in the archive matches <span class="font-mono text-zinc-300">%s</span>.</p>
			</div></div>`, html.EscapeString(data.Query))
			_, err := io.WriteString(w, b.String())
			return err
		}

		// Results count + backend badge.
		fmt.Fprintf(&b, `<div class="mb-3 flex items-center justify-between gap-3">
			<p class="font-mono text-xs text-mute"><span class="text-zinc-200">%d</span> result(s) for <span class="text-zinc-200">%s</span></p>
			<span class="rounded-md bg-white/5 px-2 py-0.5 font-mono text-[11px] uppercase tracking-widest text-zinc-400">%s</span>
		</div>`, len(data.Results), html.EscapeString(data.Query), html.EscapeString(data.Backend))

		b.WriteString(`<div class="overflow-hidden rounded-2xl border border-white/10 bg-ink-900/60 shadow-2xl shadow-black/40">
			<div class="overflow-x-auto">
			<table class="w-full min-w-[640px] text-sm">
				<thead><tr class="border-b border-white/10 text-left font-mono text-[11px] uppercase tracking-widest text-mute">
					<th class="px-5 py-3.5 font-medium">Date</th>
					<th class="px-5 py-3.5 font-medium">From</th>
					<th class="px-5 py-3.5 font-medium">Subject</th>
					<th class="px-5 py-3.5 font-medium">Folder</th>
					<th class="px-5 py-3.5 text-right font-medium">Actions</th>
				</tr></thead>
				<tbody class="divide-y divide-white/5">`)
		for _, r := range data.Results {
			fmt.Fprintf(&b, `<tr class="group transition-colors hover:bg-white/[0.025]">
				<td class="px-5 py-3.5 font-mono text-xs text-zinc-300">%s</td>
				<td class="px-5 py-3.5 text-xs text-zinc-300">%s</td>
				<td class="px-5 py-3.5 text-xs text-zinc-200">%s</td>
				<td class="px-5 py-3.5"><span class="rounded-md bg-white/5 px-1.5 py-0.5 font-mono text-[11px] text-zinc-300">%s</span></td>
				<td class="px-5 py-3.5 text-right">
					<a href="/browse/download/%d" class="inline-flex items-center gap-1.5 rounded-lg border border-white/10 px-3 py-1.5 text-xs font-medium text-zinc-300 transition hover:border-ketchup/50 hover:bg-ketchup/10 hover:text-white">
						<svg class="h-3.5 w-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>
						Download
					</a>
				</td>
			</tr>`, html.EscapeString(shortDate(r.Date)), html.EscapeString(orDash(r.From)), html.EscapeString(orDash(r.Subject)), html.EscapeString(orDash(r.Folder)), r.ID)
		}
		b.WriteString(`</tbody></table></div></div></div>`)

		_, err := io.WriteString(w, b.String())
		return err
	})
	return layout.Document("Search", "search", inner)
}

func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
