package provider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"skim/internal/domain"
)

var hunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)

type hunk struct {
	left, leftCount, right, rightCount int
}

// parsePatch checks the entire patch, not just its headers. A truncated last
// hunk must not turn nonexistent lines into selectable review anchors.
func parsePatch(patch string) ([]hunk, int, int, error) {
	var hunks []hunk
	var additions, deletions, leftSeen, rightSeen int
	maxInt := int(^uint(0) >> 1)
	finish := func() bool {
		return len(hunks) == 0 || (leftSeen == hunks[len(hunks)-1].leftCount && rightSeen == hunks[len(hunks)-1].rightCount)
	}
	lines := strings.Split(patch, "\n")
	for i, line := range lines {
		if line == "" && i == len(lines)-1 {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if !finish() {
				return nil, 0, 0, fmt.Errorf("incomplete diff hunk")
			}
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, 0, 0, fmt.Errorf("invalid diff hunk header")
			}
			values := [4]int{}
			for j := range values {
				if (j == 1 || j == 3) && m[j+1] == "" {
					values[j] = 1
					continue
				}
				n, err := strconv.Atoi(m[j+1])
				if err != nil {
					return nil, 0, 0, fmt.Errorf("diff line number is out of range")
				}
				values[j] = n
			}
			h := hunk{values[0], values[1], values[2], values[3]}
			if h.left > maxInt-h.leftCount || h.right > maxInt-h.rightCount ||
				(h.leftCount > 0 && h.left == 0) || (h.rightCount > 0 && h.right == 0) ||
				(h.leftCount == 0 && h.rightCount == 0) {
				return nil, 0, 0, fmt.Errorf("invalid diff line range")
			}
			if len(hunks) > 0 {
				prev := hunks[len(hunks)-1]
				if h.left < prev.left+prev.leftCount || h.right < prev.right+prev.rightCount {
					return nil, 0, 0, fmt.Errorf("overlapping diff hunks")
				}
			}
			hunks = append(hunks, h)
			leftSeen, rightSeen = 0, 0
			continue
		}
		if len(hunks) == 0 || line == "" {
			return nil, 0, 0, fmt.Errorf("invalid diff content")
		}
		switch line[0] {
		case ' ':
			leftSeen++
			rightSeen++
		case '+':
			rightSeen++
			additions++
		case '-':
			leftSeen++
			deletions++
		case '\\':
			if line != `\ No newline at end of file` || i == 0 || strings.HasPrefix(lines[i-1], "@@") || strings.HasPrefix(lines[i-1], `\`) {
				return nil, 0, 0, fmt.Errorf("invalid diff newline marker")
			}
		default:
			return nil, 0, 0, fmt.Errorf("invalid diff content")
		}
		h := hunks[len(hunks)-1]
		if leftSeen > h.leftCount || rightSeen > h.rightCount {
			return nil, 0, 0, fmt.Errorf("diff content exceeds hunk range")
		}
	}
	if len(hunks) == 0 || !finish() {
		return nil, 0, 0, fmt.Errorf("missing or incomplete diff hunks")
	}
	return hunks, additions, deletions, nil
}

func patchStatus(patch string, additions, deletions int) string {
	if patch == "" {
		return "unavailable"
	}
	_, added, deleted, err := parsePatch(patch)
	if err != nil || added != additions || deleted != deletions {
		return "truncated"
	}
	return "available"
}

// ValidateAnchor requires a complete patch and a positive inclusive range on
// one side of one hunk. Renamed files are addressed by their current path.
func ValidateAnchor(files []domain.File, path string, start, end int, side string) error {
	if path == "" || start < 1 || end < start || (side != "LEFT" && side != "RIGHT") {
		return fmt.Errorf("invalid review anchor: require a path, positive ordered lines, and LEFT or RIGHT side")
	}
	for _, file := range files {
		if file.Path != path {
			continue
		}
		if file.PatchStatus != "available" {
			return fmt.Errorf("review anchor requires a complete available patch")
		}
		hunks, additions, deletions, err := parsePatch(file.Patch)
		if err != nil || additions != file.Additions || deletions != file.Deletions {
			return fmt.Errorf("review anchor cannot use an incomplete or malformed patch")
		}
		for _, h := range hunks {
			line, count := h.right, h.rightCount
			if side == "LEFT" {
				line, count = h.left, h.leftCount
			}
			if count > 0 && start >= line && end-line < count {
				return nil
			}
		}
		return fmt.Errorf("review anchor must reference contiguous lines on the selected side within one diff hunk")
	}
	return fmt.Errorf("review anchor path is not in the pull request")
}
