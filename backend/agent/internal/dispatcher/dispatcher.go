package dispatcher

import (
	"regexp"
	"strings"
)

type PathType int

const (
	PathFast PathType = iota
	PathSlow
	PathBlocked
)

type Dispatcher struct {
	blacklist    []*regexp.Regexp
	naturalLangRe *regexp.Regexp
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		blacklist: []*regexp.Regexp{
			regexp.MustCompile(`rm -rf /`),
			regexp.MustCompile(`fdisk`),
			regexp.MustCompile(`mkfs`),
		},
		// Natural language heuristic: if it contains only letters and spaces and is relatively long
		naturalLangRe: regexp.MustCompile(`^[\p{L}\s]{10,}$`),
	}
}

func (d *Dispatcher) Classify(command string) PathType {
	lower := strings.ToLower(strings.TrimSpace(command))

	// Check blacklist first
	for _, re := range d.blacklist {
		if re.MatchString(lower) {
			return PathBlocked
		}
	}

	// Check if it looks like natural language (Simplified heuristic)
	// If it contains spaces and no typical shell markers like / . - it might be NL
	if d.naturalLangRe.MatchString(command) && !strings.ContainsAny(command, "/.-_") {
		return PathSlow
	}

	return PathFast
}
