// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadMapping reads the file -org-subjects names: Drive's organization ids
// against the subjects the platform's identity provider assigns them.
//
// Two shapes, chosen by what the file begins with rather than by its name, so
// a file saved under either extension reads:
//
//	{"7b1d…": "https://issuer.example|org-acme"}
//
//	organization,subject
//	7b1d…,https://issuer.example|org-acme
//
// The CSV header is optional and is recognized by its first field. Every row
// needs both columns; a blank line is skipped and anything else is an error,
// because a mapping read half way is a copy that refuses part way through.
func ReadMapping(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("migrate-drive: read the organization mapping: %w", err)
	}
	m, err := parseMapping(body)
	if err != nil {
		return nil, fmt.Errorf("migrate-drive: read %s: %w", path, err)
	}
	return m, nil
}

// parseMapping reads either shape from the bytes of the file.
func parseMapping(body []byte) (map[string]string, error) {
	head := strings.TrimLeftFunc(string(body), isSpace)
	if strings.HasPrefix(head, "{") || strings.HasPrefix(head, "[") {
		return parseJSONMapping(body)
	}
	return parseCSVMapping(body)
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func parseJSONMapping(body []byte) (map[string]string, error) {
	m := map[string]string{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("the JSON is not an object of organization id against subject: %w", err)
	}
	for id, subject := range m {
		if err := checkPair(id, subject); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func parseCSVMapping(body []byte) (map[string]string, error) {
	r := csv.NewReader(strings.NewReader(string(body)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	m := map[string]string{}
	for line := 1; ; line++ {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the CSV does not parse: %w", err)
		}
		if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
			continue
		}
		if len(record) != 2 {
			return nil, fmt.Errorf("line %d holds %d fields; a row is organization,subject", line, len(record))
		}
		id, subject := strings.TrimSpace(record[0]), strings.TrimSpace(record[1])
		if line == 1 && id == "organization" {
			continue // the optional header
		}
		if err := checkPair(id, subject); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		m[id] = subject
	}
	return m, nil
}

// checkPair refuses a row that names nothing. An empty subject would become an
// owner column no authorizer answers for, which is worse than a refusal.
func checkPair(id, subject string) error {
	if id == "" {
		return fmt.Errorf("an organization id is empty")
	}
	if subject == "" {
		return fmt.Errorf("the organization %s is mapped to an empty subject", id)
	}
	return nil
}
