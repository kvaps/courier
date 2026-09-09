package api

import (
	"strings"
)

// DefaultReplyPrompt closes a question with the shortest possible invitation to
// answer, so the reader can reply with one word instead of composing a reply.
// It is a channel setting rather than a constant in the renderer because the
// language a person is addressed in belongs to the channel, not to the daemon.
const DefaultReplyPrompt = "OK?"

// Render lays a Body out as the plain text a person reads.
//
// It is deliberately almost no formatting: a progress marker, the sender's own
// lines separated by blank lines, an arrow before the proposal, and the reply
// prompt at the end. The renderer does not write sentences of its own — it has
// no idea what the message is about and would only add filler between the
// reader and the decision. What it does enforce is the running order, which is
// the part senders get wrong: re-orientation before the choice, the choice
// before the recommendation.
//
// The result carries no markup. Agent-authored text is full of backticks,
// asterisks and underscores; asking a transport to parse it as Markdown turns a
// stray character into a delivery failure of the very message someone was
// waiting on.
func (b Body) Render(replyPrompt string, awaitReply bool) string {
	var parts []string

	head := strings.TrimSpace(b.Context)
	if p := b.Progress.Prefix(); p != "" {
		if head == "" {
			head = p
		} else {
			head = p + " — " + head
		}
	}
	if head != "" {
		parts = append(parts, head)
	}
	if q := strings.TrimSpace(b.Question); q != "" {
		parts = append(parts, q)
	}
	if t := strings.TrimSpace(b.Text); t != "" {
		parts = append(parts, t)
	}
	if p := strings.TrimSpace(b.Proposal); p != "" {
		parts = append(parts, "→ "+p)
	}
	if awaitReply {
		if prompt := strings.TrimSpace(replyPrompt); prompt != "" {
			parts = append(parts, prompt)
		}
	}
	return strings.Join(parts, "\n\n")
}

// DefaultChoicePrompt closes a drafted answer by saying, in one line, that the
// buttons are an offer and not the only way through: the reader can always
// write their own words instead. Like DefaultReplyPrompt it is a channel
// setting, because the language a person is addressed in belongs to the channel.
const DefaultChoicePrompt = "Tap one — or write your own answer."

// RenderDraft lays out an answer somebody drafted for the reader to confirm.
//
// It shows the words of every option in full, not just the button labels. That
// is the whole safeguard: a button says "Send", and a reader who taps it
// without having seen what gets sent has approved nothing. It also names the
// drafter above the options, so an answer written for the reader can never be
// mistaken for a line they wrote themselves.
func RenderDraft(b Body, draftedBy string, choices []Choice, prompt string) string {
	parts := draftParts(b, draftedBy, choices)
	if p := strings.TrimSpace(prompt); p != "" {
		parts = append(parts, p)
	}
	return strings.Join(parts, "\n\n")
}

// RenderChosen is the draft as it looks once a choice has been taken: the same
// offer, with the buttons gone and a line saying who took which option. The
// options that were not taken stay on screen — the record of what was on offer
// is half of what makes an approval reviewable afterwards.
//
// The outcome goes first, and that placement is the whole point. Editing a
// message raises no notification: the reader is looking at the same screen they
// were before, and a mark appended after several paragraphs of options is not
// something they will find. Someone who cannot tell whether their tap did
// anything presses again.
func RenderChosen(b Body, draftedBy string, choices []Choice, taken Choice, by string) string {
	who := strings.TrimSpace(by)
	if who == "" {
		who = "the operator"
	}
	head := "✓ " + who + " chose \"" + taken.Label + "\" — sent as the answer"
	return strings.Join(append([]string{head}, draftParts(b, draftedBy, choices)...), "\n\n")
}

// RenderRetired is the draft with its buttons withdrawn and the reason on it,
// for when the question it answered was settled some other way. The reason
// leads, for the same reason the outcome does above.
func RenderRetired(b Body, draftedBy string, choices []Choice, note string) string {
	n := strings.TrimSpace(note)
	if n == "" {
		n = "withdrawn"
	}
	head := "— " + n + " (nothing was sent)"
	return strings.Join(append([]string{head}, draftParts(b, draftedBy, choices)...), "\n\n")
}

func draftParts(b Body, draftedBy string, choices []Choice) []string {
	var parts []string
	if head := strings.TrimSpace(b.Context); head != "" {
		parts = append(parts, head)
	}
	if by := strings.TrimSpace(draftedBy); by != "" {
		parts = append(parts, "Draft by "+by+":")
	}
	if t := strings.TrimSpace(b.Text); t != "" {
		parts = append(parts, t)
	}
	for _, c := range choices {
		parts = append(parts, "▸ "+c.Label+" — "+strings.TrimSpace(c.Answer))
	}
	return parts
}

// Prefix renders a progress marker like "3/12", or "" when there is none.
// A total of zero or an index past the total is treated as no progress at all:
// a wrong counter is worse than none, because it tells the reader the run is
// nearly over when it is not.
func (p *Progress) Prefix() string {
	if p == nil || p.Total <= 0 || p.Index <= 0 || p.Index > p.Total {
		return ""
	}
	return itoa(p.Index) + "/" + itoa(p.Total)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Empty reports whether a body would render to nothing. A message must never be
// that: it wastes a notification and, on a question, leaves the reader nothing
// to answer. A message carrying only a file is not empty — the file is the
// content — which is why the caller checks attachments alongside this.
func (b Body) Empty() bool {
	return strings.TrimSpace(b.Context) == "" &&
		strings.TrimSpace(b.Question) == "" &&
		strings.TrimSpace(b.Text) == "" &&
		strings.TrimSpace(b.Proposal) == ""
}

// Summary is a one-line description of a body, for logs and for listing a
// conversation without printing every message in full.
func (b Body) Summary() string {
	for _, s := range []string{b.Question, b.Text, b.Context, b.Proposal} {
		if t := strings.TrimSpace(firstLine(s)); t != "" {
			return truncate(t, 120)
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
