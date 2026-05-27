package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"
)



type ResourceKey struct {
	Kind      string
	Namespace string
	Name      string
}

type Resource struct {
	Key          ResourceKey
	FilePath     string
	RawYAML      string
	Dependencies []ResourceKey
}

type K8sManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		DependsOn []struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"dependsOn"`
	} `yaml:"spec"`
}



type Index struct {
	Resources map[ResourceKey]*Resource
}

func NewIndex() *Index {
	return &Index{
		Resources: make(map[ResourceKey]*Resource),
	}
}

func (idx *Index) Build(rootPath string) error {
	fmt.Fprintf(os.Stderr, "🚀 [DEBUG] Начинаем сканирование папки: %s\n", rootPath)

	return filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		name := info.Name()

		// 1. Игнорируем ВСЕ скрытые файлы и папки (начинаются с точки)
		// Проверка name != "." нужна, чтобы не пропустить корень проекта
		if strings.HasPrefix(name, ".") && name != "." {
			if info.IsDir() {
				return filepath.SkipDir // Полностью пропускаем скрытую папку
			}
			return nil // Пропускаем скрытый файл
		}

		// 2. Игнорируем папки с зависимостями мусором
		if info.IsDir() {
			if name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		// 3. Берем только yaml/yml
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// 4. Парсим Multi-document YAML
		decoder := yaml.NewDecoder(bytes.NewReader(content))
		for {
			var node yaml.Node
			err := decoder.Decode(&node)
			if err == io.EOF {
				break
			}
			if err != nil {
				continue
			}

			var buf bytes.Buffer
			yaml.NewEncoder(&buf).Encode(&node)
			rawDoc := buf.String()

			var manifest K8sManifest
			if err := node.Decode(&manifest); err != nil {
				continue
			}

			if manifest.Kind == "" || manifest.Metadata.Name == "" {
				continue
			}

			ns := manifest.Metadata.Namespace
			if ns == "" {
				ns = "default"
			}

			key := ResourceKey{Kind: manifest.Kind, Namespace: ns, Name: manifest.Metadata.Name}

			res := &Resource{
				Key:      key,
				FilePath: path,
				RawYAML:  rawDoc,
			}

			for _, dep := range manifest.Spec.DependsOn {
				depNs := dep.Namespace
				if depNs == "" {
					depNs = ns
				}
				res.Dependencies = append(res.Dependencies, ResourceKey{
					Kind:      manifest.Kind,
					Namespace: depNs,
					Name:      dep.Name,
				})
			}

			idx.Resources[key] = res
		}
		return nil
	})
}


func getArgs(req mcp.CallToolRequest) map[string]interface{} {
	if args, ok := req.Params.Arguments.(map[string]interface{}); ok {
		return args
	}
	return make(map[string]interface{})
}



func main() {
	projectRoot := os.Getenv("PROJECT_ROOT")
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}


	var index *Index
	var indexLoaded bool


	getIndex := func() (*Index, error) {
		if indexLoaded {
			return index, nil
		}

		idx := NewIndex()

		fmt.Fprintf(os.Stderr, "Lazy indexing started for: %s\n", projectRoot)

		if err := idx.Build(projectRoot); err != nil {
			return nil, err
		}

		index = idx
		indexLoaded = true
		fmt.Fprintf(os.Stderr, "Lazy indexing finished. Found %d resources.\n", len(index.Resources))
		return index, nil
	}

	mcpServer := server.NewMCPServer("flux-yaml-indexer", "1.0.1")


	mcpServer.AddTool(mcp.Tool{
		Name:        "list_resources",
		Description: "List all GitOps/Kubernetes resources in the repository grouped by Kind.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"kind": map[string]interface{}{"type": "string", "description": "Filter by kind"},
			},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		idx, err := getIndex()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Error building index: %v", err)), nil
		}

		args := getArgs(request)
		filterKind := ""
		if val, ok := args["kind"].(string); ok {
			filterKind = val
		}

		var result strings.Builder
		count := 0
		for key, res := range idx.Resources {
			if filterKind == "" || strings.EqualFold(key.Kind, filterKind) {
				result.WriteString(fmt.Sprintf("- [%s] %s/%s (File: %s)\n", key.Kind, key.Namespace, key.Name, res.FilePath))
				count++
			}
		}
		return mcp.NewToolResultText(fmt.Sprintf("Found %d resources:\n%s", count, result.String())), nil
	})


	mcpServer.AddTool(mcp.Tool{
		Name:        "get_resource_yaml",
		Description: "Get the exact, raw YAML of a specific resource. Use this instead of reading files.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"kind":      map[string]interface{}{"type": "string"},
				"name":      map[string]interface{}{"type": "string"},
				"namespace": map[string]interface{}{"type": "string", "default": "default"},
			},
			Required: []string{"kind", "name"},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		idx, err := getIndex()
		if err != nil {
			return mcp.NewToolResultText("Error building index"), nil
		}

		args := getArgs(request)
		kind, _ := args["kind"].(string)
		name, _ := args["name"].(string)
		ns, ok := args["namespace"].(string)
		if !ok || ns == "" {
			ns = "default"
		}

		key := ResourceKey{Kind: kind, Namespace: ns, Name: name}
		if res, exists := idx.Resources[key]; exists {
			return mcp.NewToolResultText(fmt.Sprintf("# Extracted from: %s\n%s", res.FilePath, res.RawYAML)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Resource %s/%s of kind %s not found.", ns, name, kind)), nil
	})


	mcpServer.AddTool(mcp.Tool{
		Name:        "get_flux_dependencies",
		Description: "Get upstream resources this resource depends on.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"kind":      map[string]interface{}{"type": "string"},
				"name":      map[string]interface{}{"type": "string"},
				"namespace": map[string]interface{}{"type": "string"},
			},
			Required: []string{"kind", "name", "namespace"},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		idx, err := getIndex()
		if err != nil {
			return mcp.NewToolResultText("Error building index"), nil
		}

		args := getArgs(request)
		kind, _ := args["kind"].(string)
		name, _ := args["name"].(string)
		ns, _ := args["namespace"].(string)

		key := ResourceKey{Kind: kind, Namespace: ns, Name: name}
		res, exists := idx.Resources[key]
		if !exists {
			return mcp.NewToolResultText("Resource not found."), nil
		}

		if len(res.Dependencies) == 0 {
			return mcp.NewToolResultText("No explicit dependencies found."), nil
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf("%s %s/%s depends on:\n", kind, ns, name))
		for _, dep := range res.Dependencies {
			b.WriteString(fmt.Sprintf("- [%s] %s/%s\n", dep.Kind, dep.Namespace, dep.Name))
		}
		return mcp.NewToolResultText(b.String()), nil
	})

	if err := server.ServeStdio(mcpServer); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
