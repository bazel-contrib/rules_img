package expandtemplate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/template"
)

// request represents the input JSON for template expansion
type request struct {
	BuildSettings map[string]buildSetting    `json:"build_settings"`
	Templates     map[string]json.RawMessage `json:"templates"`
}

// buildSetting represents the "value" of the Bazel skylibs' BuildSettingInfo provider.
type buildSetting struct {
	value any
}

func (bs *buildSetting) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("unmarshaling build setting: %w", err)
	}

	// upcast float to int if possible
	if f, ok := value.(float64); ok && f == float64(int(f)) {
		value = int(f)
	}
	bs.value = value

	switch v := value.(type) {
	case string, int, bool, []string:
		// Supported types
	default:
		return fmt.Errorf("unsupported build setting type: %v of type %T", value, v)
	}

	return nil
}

func (bs *buildSetting) MarshalJSON() ([]byte, error) {
	return json.Marshal(bs.value)
}

type buildSettings map[string]buildSetting

func (bs buildSettings) AsTemplateData() map[string]any {
	data := make(map[string]any, len(bs))
	for k, v := range bs {
		data[k] = v.value
	}
	return data
}

// ExpandTemplateProcess is the main entry point for the expand-template subcommand
func ExpandTemplateProcess(ctx context.Context, args []string) {
	// Define flags for stamp files
	var stampFiles []string
	flagSet := flag.NewFlagSet("expand-template", flag.ExitOnError)
	flagSet.Func("stamp", "Path to a stamp file (can be specified multiple times)", func(s string) error {
		stampFiles = append(stampFiles, s)
		return nil
	})

	// Parse flags
	if err := flagSet.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	// Get positional arguments
	args = flagSet.Args()
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: img expand-template [--stamp file]... <input.json> <output.json>\n")
		os.Exit(1)
	}

	inputPath := args[0]
	outputPath := args[1]

	if err := expandTemplates(inputPath, outputPath, stampFiles); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func expandTemplates(inputPath, outputPath string, stampFiles []string) error {
	// Read input JSON
	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("reading input file: %w", err)
	}

	// Create template data map
	buildSettings := make(buildSettings)

	// Read stamp files and add their key-value pairs
	for _, stampFile := range stampFiles {
		if err := readStampFile(stampFile, buildSettings); err != nil {
			return fmt.Errorf("reading stamp file %s: %w", stampFile, err)
		}
	}

	var request request
	if err := json.Unmarshal(inputData, &request); err != nil {
		return fmt.Errorf("parsing input JSON: %w", err)
	}

	// Add build settings to template data
	for k, v := range request.BuildSettings {
		buildSettings[k] = v
	}

	templateData := buildSettings.AsTemplateData()
	output := make(map[string]json.RawMessage)

	// Expand each template
	for key, rawValue := range request.Templates {
		var valueStr string
		if err := json.Unmarshal(rawValue, &valueStr); err == nil {
			// Single string template
			expanded, err := expandTemplate(valueStr, templateData)
			if err != nil {
				return fmt.Errorf("expanding template for key %q: %w", key, err)
			}
			output[key] = json.RawMessage(fmt.Sprintf("%q", expanded))
			continue
		}

		var valueList []string
		if err := json.Unmarshal(rawValue, &valueList); err == nil {
			// List of strings template
			expandedList := make([]string, len(valueList))
			for i, v := range valueList {
				expanded, err := expandTemplate(v, templateData)
				if err != nil {
					return fmt.Errorf("expanding template for key %q index %d: %w", key, i, err)
				}
				expandedList[i] = expanded
			}

			if key == "tags" {
				// post-process tags to remove empty tags and duplicates
				slices.Sort(expandedList)
				expandedList = slices.Compact(expandedList)
			}

			marshaledList, err := json.Marshal(expandedList)
			if err != nil {
				return fmt.Errorf("marshaling expanded list for key %q: %w", key, err)
			}
			output[key] = json.RawMessage(marshaledList)
			continue
		}

		var valueMap map[string]string
		if err := json.Unmarshal(rawValue, &valueMap); err == nil {
			// Map of string to string template
			expandedMap := make(map[string]string)
			for k, v := range valueMap {
				expanded, err := expandTemplate(v, templateData)
				if err != nil {
					return fmt.Errorf("expanding template for key %q map key %q: %w", key, k, err)
				}
				expandedMap[k] = expanded
			}

			marshaledMap, err := json.Marshal(expandedMap)
			if err != nil {
				return fmt.Errorf("marshaling expanded map for key %q: %w", key, err)
			}
			output[key] = json.RawMessage(marshaledMap)
			continue
		}

		return fmt.Errorf("template value for key %q is neither a string, list of strings, nor map of strings", key)
	}

	// Write output JSON
	outputData, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling output: %w", err)
	}
	if err := os.WriteFile(outputPath, outputData, 0644); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}
	return nil
}

func expandTemplate(tmplStr string, data map[string]any) (string, error) {
	if tmplStr == "" {
		return "", nil
	}

	tmpl, err := template.New("expand").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return buf.String(), nil
}

// readStampFile reads a Bazel stamp file and adds key-value pairs to the data map
func readStampFile(path string, data buildSettings) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening stamp file: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first space to get key and value
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			key := parts[0]
			value := parts[1]
			// always interpret as string
			data[key] = buildSetting{value: value}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stamp file: %w", err)
	}

	return nil
}
