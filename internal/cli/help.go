package cli

import (
	"regexp"
	"strings"
)

// The usage text is one constant, and a person asking about one command does
// not want the other sixty (spec 004 #38). Rather than a second copy of every
// synopsis, which would drift from the first, the help for a command is read
// out of Usage: the synopsis blocks that begin with it.

// synopsisStart finds the first line of a synopsis block: two spaces, `tracepad`
// and the command words. Group names may be hyphenated (`score-configs`).
var synopsisStart = regexp.MustCompile(`^  tracepad ([a-z][a-z-]*(?: [a-z][a-z-]*)?)`)

// usageFor is the help for a command: every synopsis block in Usage that
// begins with topic (`keys create`, or `keys` for the group), the connection
// flags that every command takes, and — when a block marks the command as
// needing the admin token — where the CLI finds it. A topic with no block of
// its own falls back to the group it belongs to, and one with no group at all
// to the whole text, which is what the CLI printed for everything before.
func usageFor(topic string) string {
	words := strings.Fields(topic)
	for ; len(words) > 0; words = words[:len(words)-1] {
		if blocks := synopsisBlocks(words); blocks != "" {
			footer := "\nConnection: --url URL (env TRACEPAD_URL), --key KEY (env TRACEPAD_API_KEY), --json.\n"
			if strings.Contains(blocks, "admin token") {
				footer += "(admin token): the admin token goes in --key or TRACEPAD_API_KEY; with neither set,\n" +
					"TRACEPAD_ADMIN_TOKEN (or the file TRACEPAD_ADMIN_TOKEN_FILE names) is used for a\n" +
					"server on this machine (localhost, 127.0.0.0/8, ::1) and for no other.\n"
			}
			return blocks + footer + "\n`tracepad --help` lists every command.\n"
		}
	}
	return Usage
}

// synopsisBlocks collects the blocks of Usage whose command words start with
// words. A block is its first line and the indented lines that continue it, up
// to a blank line or the next block.
func synopsisBlocks(words []string) string {
	var out strings.Builder
	keep := false
	for _, line := range strings.Split(Usage, "\n") {
		if match := synopsisStart.FindStringSubmatch(line); match != nil {
			keep = startsWith(strings.Fields(match[1]), words)
		} else if strings.TrimSpace(line) == "" {
			keep = false
		}
		if keep {
			out.WriteString(line + "\n")
		}
	}
	return out.String()
}

// startsWith says whether the block's command words begin with the topic's.
func startsWith(command, topic []string) bool {
	if len(command) < len(topic) {
		return false
	}
	for i, word := range topic {
		if command[i] != word {
			return false
		}
	}
	return true
}
