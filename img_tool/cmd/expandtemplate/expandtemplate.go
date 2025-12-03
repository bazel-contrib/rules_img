package expandtemplate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"text/template"
)

// request represents the input JSON for template expansion
type request struct {
	BuildSettings              map[string]buildSetting    `json:"build_settings"`
	Templates                  map[string]json.RawMessage `json:"templates"`
	NewlineDelimitedListsFiles map[string]string          `json:"newline_delimited_lists_files,omitempty"`
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
	// Define flags for stamp files and JSON variables
	var stampFiles []string
	var jsonVars []string
	var exposeKVs []string
	flagSet := flag.NewFlagSet("expand-template", flag.ExitOnError)
	flagSet.Func("stamp", "Path to a stamp file (can be specified multiple times)", func(s string) error {
		stampFiles = append(stampFiles, s)
		return nil
	})
	flagSet.Func("json-var", "Map JSON file into template data (format: path.to.key=file.json)", func(s string) error {
		jsonVars = append(jsonVars, s)
		return nil
	})
	flagSet.Func("expose-kv", "Expose keys from a KV array as template variables (format: path.to.kvarray)", func(s string) error {
		exposeKVs = append(exposeKVs, s)
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
		fmt.Fprintf(os.Stderr, "Usage: img expand-template [--stamp file]... [--json-var path=file.json]... [--expose-kv path.to.kvarray]... <input.json> <output.json>\n")
		os.Exit(1)
	}

	inputPath := args[0]
	outputPath := args[1]

	if err := expandTemplates(inputPath, outputPath, stampFiles, jsonVars, exposeKVs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func expandTemplates(inputPath, outputPath string, stampFiles []string, jsonVars []string, exposeKVs []string) error {
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

	// Read newline-delimited list files and merge them into templates
	if err := mergeNewlineDelimitedFiles(&request); err != nil {
		return fmt.Errorf("merging newline-delimited files: %w", err)
	}

	// Add build settings to template data
	maps.Copy(buildSettings, request.BuildSettings)

	templateData := buildSettings.AsTemplateData()

	// Process JSON variables into a separate map with case-insensitive keys
	jsonVarData := make(map[string]any)
	for _, jsonVar := range jsonVars {
		if err := processJSONVar(jsonVar, jsonVarData); err != nil {
			return fmt.Errorf("processing json-var %q: %w", jsonVar, err)
		}
	}

	// Normalize JSON var data to lowercase keys for case-insensitive access
	jsonVarData = makeCaseInsensitiveMap(jsonVarData)

	// Merge: start with jsonVarData, then overlay templateData (allowing overrides)
	finalData := make(map[string]any)
	maps.Copy(finalData, jsonVarData)
	maps.Copy(finalData, templateData)
	templateData = finalData
	// Expose keys from KV arrays as template variables
	prelude, err := variableDefinitionPrelude(templateData, exposeKVs)
	if err != nil {
		// if an error occurs here, it most likely indicates a missing path in the template data.
		// this should be handled gracefully.
		prelude = ""
	}
	output := make(map[string]json.RawMessage)

	// Expand each template
	for key, rawValue := range request.Templates {
		var valueStr string
		if err := json.Unmarshal(rawValue, &valueStr); err == nil {
			// Single string template
			templateStr := prelude + valueStr
			expanded, err := expandTemplate(templateStr, templateData)
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
				templateStr := prelude + v
				expanded, err := expandTemplate(templateStr, templateData)
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
				templateStr := prelude + v
				expanded, err := expandTemplate(templateStr, templateData)
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
	if err := os.WriteFile(outputPath, outputData, 0o644); err != nil {
		return fmt.Errorf("writing output file: %w", err)
	}
	return nil
}

func expandTemplate(tmplStr string, data map[string]any) (string, error) {
	if tmplStr == "" {
		return "", nil
	}

	// Add custom template functions
	funcMap := template.FuncMap{
		"getkv":      getKVFromArray,
		"appendkv":   appendKV,
		"prependkv":  prependKV,
		"split":      strings.Split,
		"join":       strings.Join,
		"hasprefix":  strings.HasPrefix,
		"hassuffix":  strings.HasSuffix,
		"trimprefix": strings.TrimPrefix,
		"trimsuffix": strings.TrimSuffix,
	}

	tmpl, err := template.New("expand").Funcs(funcMap).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return buf.String(), nil
}

func defineVariable(varName, val string) string {
	return fmt.Sprintf("{{- $%s := %q -}}\n", varName, val)
}

func extractKeysFromKVArray(kvArray any) []string {
	var keys []string

	arr, ok := kvArray.([]any)
	if !ok {
		return keys
	}

	for _, item := range arr {
		if str, ok := item.(string); ok {
			parts := strings.SplitN(str, "=", 2)
			if len(parts) == 2 {
				keys = append(keys, parts[0])
			}
		}
	}

	slices.Sort(keys)
	return keys
}

func singleVariableDefinition(builder *strings.Builder, templateData map[string]any, exposeKV string) error {
	parts := strings.Split(exposeKV, ".")
	if len(parts) == 0 {
		return errors.New("empty expose-kv path")
	}
	// Navigate to the KV array
	current := templateData
	for i, part := range parts {
		if i == len(parts)-1 {
			// Last part: should be the KV array
			kvArray, ok := current[part]
			if !ok {
				return fmt.Errorf("path %q not found in template data", exposeKV)
			}

			// Extract keys from the KV array
			keys := extractKeysFromKVArray(kvArray)
			for _, key := range keys {
				builder.WriteString(defineVariable(key, getKVFromArray(kvArray, key)))
			}
		} else {
			// Intermediate part: navigate deeper
			next, ok := current[part].(map[string]any)
			if !ok {
				return fmt.Errorf("path %q not found in template data", exposeKV)
			}
			current = next
		}
	}
	return nil
}

func variableDefinitionPrelude(templateData map[string]any, exposeKVs []string) (string, error) {
	var prelude strings.Builder
	for _, exposeKV := range exposeKVs {
		if err := singleVariableDefinition(&prelude, templateData, exposeKV); err != nil {
			return "", fmt.Errorf("defining variables for %q: %w", exposeKV, err)
		}
	}
	return prelude.String(), nil
}

// getKVFromArray extracts an value from an OCI-style key-value pair array.
// The array contains strings in "KEY=VALUE" format.
// Returns the value if found, empty string otherwise.
func getKVFromArray(kvArray any, key string) string {
	// Handle []any (from JSON unmarshaling)
	if arr, ok := kvArray.([]any); ok {
		prefix := key + "="
		for _, item := range arr {
			if str, ok := item.(string); ok {
				if strings.HasPrefix(str, prefix) {
					return strings.TrimPrefix(str, prefix)
				}
			}
		}
	}

	// Handle []string
	if arr, ok := kvArray.([]string); ok {
		prefix := key + "="
		for _, str := range arr {
			if strings.HasPrefix(str, prefix) {
				return strings.TrimPrefix(str, prefix)
			}
		}
	}

	return ""
}

// appendKV finds a key-value pair in an array and appends a suffix to its value.
// Returns the modified value, or just the suffix if the key is not found.
// Example: appendkv .base.config.config.env "PATH" ":/custom/bin"
func appendKV(kvArray any, key string, suffix string) string {
	value := getKVFromArray(kvArray, key)
	return value + suffix
}

// prependKV finds a key-value pair in an array and prepends a prefix to its value.
// Returns the modified value, or just the prefix if the key is not found.
// Example: prependkv .base.config.config.env "PATH" "/custom/bin:"
func prependKV(kvArray any, key string, prefix string) string {
	value := getKVFromArray(kvArray, key)
	return prefix + value
}

// readStampFile reads a Bazel stamp file and adds key-value pairs to the data map
func readStampFile(path string, data buildSettings) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening stamp file: %w", err)
	}
	defer file.Close()

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

// readNewlineDelimitedFile reads a file with newline-delimited strings
func readNewlineDelimitedFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines
		if line != "" {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	return lines, nil
}

// mergeNewlineDelimitedFiles reads newline-delimited list files and merges them into templates
func mergeNewlineDelimitedFiles(req *request) error {
	for key, filePath := range req.NewlineDelimitedListsFiles {
		// Read the file
		lines, err := readNewlineDelimitedFile(filePath)
		if err != nil {
			return fmt.Errorf("reading file %s for key %q: %w", filePath, key, err)
		}

		// Get existing template value for this key
		existingRaw, exists := req.Templates[key]

		// Check if lines contain KEY=VALUE pairs
		isKeyValue := false
		if len(lines) > 0 {
			for _, line := range lines {
				if strings.Contains(line, "=") {
					isKeyValue = true
					break
				}
			}
		}

		if isKeyValue {
			// Handle as KEY=VALUE map
			finalMap := make(map[string]string)

			// If existing value exists, try to unmarshal as map
			if exists {
				var existingMap map[string]string
				if err := json.Unmarshal(existingRaw, &existingMap); err == nil {
					// Merge with existing map
					maps.Copy(finalMap, existingMap)
				} else {
					return fmt.Errorf("template value for key %q is not a map, but file contains KEY=VALUE pairs", key)
				}
			}

			// Parse KEY=VALUE lines and merge
			for _, line := range lines {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid KEY=VALUE format in line: %q", line)
				}
				finalMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}

			// Marshal back to JSON and update the template
			marshaled, err := json.Marshal(finalMap)
			if err != nil {
				return fmt.Errorf("marshaling merged map for key %q: %w", key, err)
			}
			req.Templates[key] = json.RawMessage(marshaled)
		} else {
			// Handle as list of strings
			var finalList []string

			if exists {
				// Try to unmarshal as a list first
				var existingList []string
				if err := json.Unmarshal(existingRaw, &existingList); err == nil {
					// Merge with existing list
					finalList = append(finalList, existingList...)
					finalList = append(finalList, lines...)
				} else {
					// Try as a single string
					var existingStr string
					if err := json.Unmarshal(existingRaw, &existingStr); err == nil {
						// Convert to list and merge
						if existingStr != "" {
							finalList = append(finalList, existingStr)
						}
						finalList = append(finalList, lines...)
					} else {
						return fmt.Errorf("template value for key %q is neither a string nor list of strings", key)
					}
				}
			} else {
				// No existing value, just use the lines from file
				finalList = lines
			}

			// Marshal back to JSON and update the template
			marshaled, err := json.Marshal(finalList)
			if err != nil {
				return fmt.Errorf("marshaling merged list for key %q: %w", key, err)
			}
			req.Templates[key] = json.RawMessage(marshaled)
		}
	}

	return nil
}

// processJSONVar parses a --json-var flag value and injects JSON data into the template data.
// Format: path.to.key=file.json
// All JSON keys are converted to lowercase for case-insensitive template access.
func processJSONVar(jsonVar string, templateData map[string]any) error {
	// Split on first '=' to separate path from file
	parts := strings.SplitN(jsonVar, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format, expected path=file.json")
	}

	path := parts[0]
	filePath := parts[1]

	// Read JSON file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("reading file %s: %w", filePath, err)
	}

	// Parse JSON into generic structure
	var jsonData any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return fmt.Errorf("parsing JSON from %s: %w", filePath, err)
	}

	// Split path into components (e.g., "parent.config" -> ["parent", "config"])
	pathParts := strings.Split(path, ".")
	if len(pathParts) == 0 {
		return fmt.Errorf("empty path")
	}

	// Navigate/create the nested structure and set the value
	current := templateData
	for i, part := range pathParts {
		if i == len(pathParts)-1 {
			// Last component: set the value with case-insensitive wrapping
			current[part] = makeCaseInsensitiveValue(jsonData)
		} else {
			// Intermediate component: create or navigate to nested map
			if existing, ok := current[part]; ok {
				if existingMap, ok := existing.(map[string]any); ok {
					current = existingMap
				} else {
					return fmt.Errorf("path component %q already exists but is not a map", part)
				}
			} else {
				newMap := make(map[string]any)
				current[part] = newMap
				current = newMap
			}
		}
	}

	return nil
}

// makeCaseInsensitiveMap recursively converts all map keys to lowercase for case-insensitive access
func makeCaseInsensitiveMap(data map[string]any) map[string]any {
	result := make(map[string]any)

	for k, v := range data {
		// Convert keys to lowercase
		lowerKey := strings.ToLower(k)
		result[lowerKey] = makeCaseInsensitiveValue(v)
	}

	return result
}

// makeCaseInsensitiveValue recursively converts values, lowercasing all map keys
func makeCaseInsensitiveValue(val any) any {
	switch v := val.(type) {
	case map[string]any:
		return makeCaseInsensitiveMap(v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = makeCaseInsensitiveValue(item)
		}
		return result
	default:
		return v
	}
}
