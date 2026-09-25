package collect

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// sample is one parsed line of Prometheus text exposition format.
type sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// parsePromText is a deliberately small parser for the exposition format.
//
// It only has to read kubelet output, where metric and label names are
// well-formed and label values are Kubernetes object names. Depending on a full
// parser would add two modules to pull in perhaps sixty lines of behavior, and
// this way the failure modes are ours.
//
// wanted filters by metric name so the (large) cadvisor payload is not fully
// materialised; a nil map keeps everything.
func parsePromText(data []byte, wanted map[string]bool) []sample {
	var out []sample
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		name, rest := splitMetricName(line)
		if name == "" {
			continue
		}
		if wanted != nil && !wanted[name] {
			continue
		}

		labels, valuePart := parseLabels(rest)

		fields := strings.Fields(valuePart)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}

		out = append(out, sample{Name: name, Labels: labels, Value: v})
	}
	return out
}

// splitMetricName returns the metric name and everything after it.
func splitMetricName(line string) (string, string) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '{' || c == ' ' || c == '\t' {
			return line[:i], line[i:]
		}
	}
	return "", ""
}

// parseLabels reads an optional {a="1",b="2"} block and returns the labels plus
// the remainder of the line.
func parseLabels(s string) (map[string]string, string) {
	s = strings.TrimLeft(s, " \t")
	if len(s) == 0 || s[0] != '{' {
		return nil, s
	}

	labels := map[string]string{}
	i := 1
	for i < len(s) && s[i] != '}' {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) || s[i] == '}' {
			break
		}

		keyStart := i
		for i < len(s) && s[i] != '=' && s[i] != '}' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			break
		}
		key := s[keyStart:i]
		i++ // consume '='

		if i >= len(s) || s[i] != '"' {
			break
		}
		i++ // consume opening quote

		var val strings.Builder
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					val.WriteByte('\n')
				case 't':
					val.WriteByte('\t')
				default:
					val.WriteByte(s[i])
				}
			} else {
				val.WriteByte(s[i])
			}
			i++
		}
		i++ // consume closing quote

		labels[key] = val.String()
	}

	if i < len(s) && s[i] == '}' {
		i++
	}
	return labels, s[i:]
}
