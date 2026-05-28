package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// ==========================================
// 1. СТРУКТУРЫ И ЛОКАЛЬНЫЙ ПАРСЕР (DESIRED STATE)
// ==========================================

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
	return filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") && name != "." {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

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

// ==========================================
// 2. KUBERNETES КЛИЕНТ (ACTUAL STATE / LAZY INIT)
// ==========================================

var (
	k8sMutex     sync.Mutex
	k8sClientset *kubernetes.Clientset
	k8sDynamic   dynamic.Interface
	k8sMapper    meta.RESTMapper
	k8sInitDone  bool
	k8sInitErr   error
)

// getK8s лениво инициализирует клиенты Kubernetes.
// Ошибка возвращается как значение, чтобы не "ронять" сервер паникой.
func getK8s() (*kubernetes.Clientset, dynamic.Interface, meta.RESTMapper, error) {
	k8sMutex.Lock()
	defer k8sMutex.Unlock()

	if k8sInitDone {
		return k8sClientset, k8sDynamic, k8sMapper, k8sInitErr
	}
	k8sInitDone = true

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		k8sInitErr = fmt.Errorf("failed to build kubeconfig: %v", err)
		return nil, nil, nil, k8sInitErr
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		k8sInitErr = fmt.Errorf("failed to create clientset: %v", err)
		return nil, nil, nil, k8sInitErr
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		k8sInitErr = fmt.Errorf("failed to create dynamic client: %v", err)
		return nil, nil, nil, k8sInitErr
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		k8sInitErr = fmt.Errorf("failed to create discovery client: %v", err)
		return nil, nil, nil, k8sInitErr
	}

	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		k8sInitErr = fmt.Errorf("failed to get API group resources: %v", err)
		return nil, nil, nil, k8sInitErr
	}

	k8sClientset = clientset
	k8sDynamic = dynClient
	k8sMapper = restmapper.NewDiscoveryRESTMapper(groupResources)

	return k8sClientset, k8sDynamic, k8sMapper, nil
}

// Хелпер для получения параметров
func getArgs(req mcp.CallToolRequest) map[string]interface{} {
	if args, ok := req.Params.Arguments.(map[string]interface{}); ok {
		return args
	}
	return make(map[string]interface{})
}

// ==========================================
// 3. ТОЧКА ВХОДА (MAIN)
// ==========================================

func main() {
	projectRoot := os.Getenv("PROJECT_ROOT")
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}

	getIndex := func() (*Index, error) {
		idx := NewIndex()
		if err := idx.Build(projectRoot); err != nil {
			return nil, err
		}
		return idx, nil
	}

	mcpServer := server.NewMCPServer("hybrid-gitops-agent", "2.0.0")

	// --- [ЛОКАЛЬНЫЙ ИНСТРУМЕНТ] 1: list_resources ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "list_resources",
		Description: "List all LOCAL GitOps/Kubernetes resources in the repository grouped by Kind.",
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
		filterKind, _ := args["kind"].(string)

		var result strings.Builder
		count := 0
		for key, res := range idx.Resources {
			if filterKind == "" || strings.EqualFold(key.Kind, filterKind) {
				result.WriteString(fmt.Sprintf("- [%s] %s/%s (File: %s)\n", key.Kind, key.Namespace, key.Name, res.FilePath))
				count++
			}
		}
		return mcp.NewToolResultText(fmt.Sprintf("Found %d local resources:\n%s", count, result.String())), nil
	})

	// --- [ЛОКАЛЬНЫЙ ИНСТРУМЕНТ] 2: get_resource_yaml ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "get_resource_yaml",
		Description: "Get the exact, raw LOCAL YAML of a specific resource from Desired State.",
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
		ns, _ := args["namespace"].(string)
		if ns == "" {
			ns = "default"
		}

		key := ResourceKey{Kind: kind, Namespace: ns, Name: name}
		if res, exists := idx.Resources[key]; exists {
			return mcp.NewToolResultText(fmt.Sprintf("# Extracted from: %s\n%s", res.FilePath, res.RawYAML)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Resource %s/%s of kind %s not found LOCALLY.", ns, name, kind)), nil
	})

	// --- [ЛОКАЛЬНЫЙ ИНСТРУМЕНТ] 3: get_helm_values ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "get_helm_values",
		Description: "Extracts ONLY the spec.values segment from a LOCAL HelmRelease.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"name":      map[string]interface{}{"type": "string"},
				"namespace": map[string]interface{}{"type": "string", "default": "default"},
			},
			Required: []string{"name"},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		idx, _ := getIndex()
		args := getArgs(request)
		name, _ := args["name"].(string)
		ns, _ := args["namespace"].(string)
		if ns == "" {
			ns = "default"
		}

		key := ResourceKey{Kind: "HelmRelease", Namespace: ns, Name: name}
		res, exists := idx.Resources[key]
		if !exists {
			return mcp.NewToolResultText(fmt.Sprintf("HelmRelease %s/%s not found.", ns, name)), nil
		}

		var tempManifest struct {
			Spec struct {
				Values interface{} `yaml:"values"`
			} `yaml:"spec"`
		}
		yaml.Unmarshal([]byte(res.RawYAML), &tempManifest)

		var buf bytes.Buffer
		yamlEncoder := yaml.NewEncoder(&buf)
		yamlEncoder.SetIndent(2)
		yamlEncoder.Encode(tempManifest.Spec.Values)

		return mcp.NewToolResultText(fmt.Sprintf("# spec.values from local HelmRelease: %s/%s\n%s", ns, name, buf.String())), nil
	})

	// =========================================================================
	// НОВЫЕ ИНСТРУМЕНТЫ (LIVE CLUSTER - ACTUAL STATE)
	// =========================================================================

	// --- [LIVE ИНСТРУМЕНТ] 1: k8s_get_status ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "k8s_get_status",
		Description: "Fetch ONLY the LIVE `.status` block of a Kubernetes resource to check its actual state.",
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
		_, dyn, mapper, err := getK8s()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Cluster unreachable: %v", err)), nil
		}

		args := getArgs(request)
		kind, _ := args["kind"].(string)
		name, _ := args["name"].(string)
		ns, _ := args["namespace"].(string)
		if ns == "" {
			ns = "default"
		}

		// Динамический поиск GroupVersionResource по Kind
		mapping, err := mapper.RESTMapping(schema.GroupKind{Kind: kind})
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Failed to find API mapping for kind '%s': %v", kind, err)), nil
		}

		obj, err := dyn.Resource(mapping.Resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Failed to get %s %s/%s: %v", kind, ns, name, err)), nil
		}

		status, found, err := unstructured.NestedMap(obj.Object, "status")
		if err != nil || !found {
			return mcp.NewToolResultText(fmt.Sprintf("%s %s/%s holds no .status block.", kind, ns, name)), nil
		}

		statusYAML, err := yaml.Marshal(status)
		if err != nil {
			return mcp.NewToolResultText("Failed to marshal status block to YAML"), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("# LIVE .status for %s %s/%s\n%s", kind, ns, name, string(statusYAML))), nil
	})

	// --- [LIVE ИНСТРУМЕНТ] 2: k8s_get_flux_errors ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "k8s_get_flux_errors",
		Description: "Scan live Flux resources (HelmRelease, Kustomization) and return ONLY those not Ready, extracting their error reasons.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"namespace": map[string]interface{}{"type": "string", "description": "Search in specific namespace (leave empty for all)"},
			},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_, dyn, mapper, err := getK8s()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Cluster unreachable: %v", err)), nil
		}

		args := getArgs(request)
		ns, _ := args["namespace"].(string)

		var output strings.Builder
		fluxKinds := []string{"HelmRelease", "Kustomization"}
		errorsFound := 0

		for _, kind := range fluxKinds {
			mapping, err := mapper.RESTMapping(schema.GroupKind{Kind: kind})
			if err != nil {
				continue // Flux CRD might not be installed
			}

			var resClient dynamic.ResourceInterface
			if ns != "" {
				resClient = dyn.Resource(mapping.Resource).Namespace(ns)
			} else {
				resClient = dyn.Resource(mapping.Resource)
			}

			list, err := resClient.List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}

			for _, item := range list.Items {
				name := item.GetName()
				itemNs := item.GetNamespace()

				conditions, found, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
				if !found {
					continue
				}

				isReady := false
				var errMsg, reason string

				for _, rawCond := range conditions {
					cond, ok := rawCond.(map[string]interface{})
					if !ok {
						continue
					}
					if cond["type"] == "Ready" {
						if cond["status"] == "True" {
							isReady = true
						} else {
							reason, _ = cond["reason"].(string)
							errMsg, _ = cond["message"].(string)
						}
						break
					}
				}

				if !isReady {
					errorsFound++
					output.WriteString(fmt.Sprintf("❌ [%s] %s/%s\n   Reason: %s\n   Message: %s\n\n", kind, itemNs, name, reason, errMsg))
				}
			}
		}

		if errorsFound == 0 {
			return mcp.NewToolResultText("✅ All Flux resources (HelmReleases/Kustomizations) are Ready."), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Found %d failing Flux resources:\n\n%s", errorsFound, output.String())), nil
	})

	// --- [LIVE ИНСТРУМЕНТ] 3: k8s_get_pod_logs ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "k8s_get_pod_logs",
		Description: "Fetch the tail logs of a pod by a prefix (due to random trailing hashes).",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"pod_prefix": map[string]interface{}{"type": "string"},
				"namespace":  map[string]interface{}{"type": "string"},
				"lines":      map[string]interface{}{"type": "number", "default": 100},
			},
			Required: []string{"pod_prefix", "namespace"},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		clientset, _, _, err := getK8s()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Cluster unreachable: %v", err)), nil
		}

		args := getArgs(request)
		prefix, _ := args["pod_prefix"].(string)
		ns, _ := args["namespace"].(string)

		linesFloat, ok := args["lines"].(float64)
		tailLines := int64(100)
		if ok {
			tailLines = int64(linesFloat)
		}

		// Ищем под по префиксу
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Failed to list pods in namespace %s: %v", ns, err)), nil
		}

		var targetPod string
		for _, p := range pods.Items {
			if strings.HasPrefix(p.Name, prefix) {
				targetPod = p.Name
				break
			}
		}

		if targetPod == "" {
			return mcp.NewToolResultText(fmt.Sprintf("No pod starting with '%s' found in namespace '%s'.", prefix, ns)), nil
		}

		// Берем логи
		logOptions := &corev1.PodLogOptions{
			TailLines: &tailLines,
		}
		req := clientset.CoreV1().Pods(ns).GetLogs(targetPod, logOptions)
		podLogs, err := req.Stream(ctx)
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Failed to open stream for pod %s: %v", targetPod, err)), nil
		}
		defer podLogs.Close()

		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, podLogs)
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Error reading logs: %v", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("## Logs for Pod: %s (Last %d lines)\n```\n%s\n```", targetPod, tailLines, buf.String())), nil
	})

	// --- [LIVE ИНСТРУМЕНТ] 4: k8s_get_events ---
	mcpServer.AddTool(mcp.Tool{
		Name:        "k8s_get_events",
		Description: "Fetch Kubernetes events to debug issues (e.g., Pod crashes, scheduling failures, Mount errors). Returns recent sorted events.",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"namespace":    map[string]interface{}{"type": "string", "description": "Namespace to check. Leave empty for all."},
				"kind":         map[string]interface{}{"type": "string", "description": "Filter by resource kind (e.g., Pod, Deployment, HelmRelease)."},
				"name":         map[string]interface{}{"type": "string", "description": "Filter by resource name (supports prefix match, very useful for finding pod events by deployment name)."},
				"warning_only": map[string]interface{}{"type": "boolean", "description": "Return ONLY Warning events to filter out noise. Default is true."},
			},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		clientset, _, _, err := getK8s()
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Cluster unreachable: %v", err)), nil
		}

		args := getArgs(request)
		ns, _ := args["namespace"].(string)
		kind, _ := args["kind"].(string)
		name, _ := args["name"].(string)

		warningOnly := true // По умолчанию только Warning
		if wo, ok := args["warning_only"].(bool); ok {
			warningOnly = wo
		}

		// Запрашиваем эвенты через стандартный CoreV1 API
		eventsList, err := clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("Failed to list events in namespace '%s': %v", ns, err)), nil
		}

		var filteredEvents []corev1.Event
		for _, ev := range eventsList.Items {
			// Фильтрация по типу
			if warningOnly && ev.Type != "Warning" {
				continue
			}
			// Фильтрация по Kind
			if kind != "" && !strings.EqualFold(ev.InvolvedObject.Kind, kind) {
				continue
			}
			// Фильтрация по Name (используем HasPrefix, чтобы при поиске Deployment=backend
			// показать эвенты подов backend-xxxx-yyyy)
			if name != "" && !strings.HasPrefix(ev.InvolvedObject.Name, name) {
				continue
			}
			filteredEvents = append(filteredEvents, ev)
		}

		if len(filteredEvents) == 0 {
			msg := "No events found."
			if warningOnly {
				msg = "No WARNING events found. Everything seems to be fine (or try setting warning_only: false)."
			}
			return mcp.NewToolResultText(msg), nil
		}

		// Сортируем от самых свежих к старым
		sort.Slice(filteredEvents, func(i, j int) bool {
			t1 := filteredEvents[i].LastTimestamp.Time
			if t1.IsZero() {
				t1 = filteredEvents[i].CreationTimestamp.Time
			}
			t2 := filteredEvents[j].LastTimestamp.Time
			if t2.IsZero() {
				t2 = filteredEvents[j].CreationTimestamp.Time
			}
			return t1.After(t2)
		})

		// Лимитируем выдачу (бережем токены)
		limit := 30
		if len(filteredEvents) < limit {
			limit = len(filteredEvents)
		}

		var output strings.Builder
		output.WriteString(fmt.Sprintf("### Top %d Recent Events (WarningOnly: %v)\n\n", limit, warningOnly))

		for _, ev := range filteredEvents[:limit] {
			count := ev.Count
			if count == 0 {
				count = 1
			}

			timeStr := ev.LastTimestamp.Time.Format("2006-01-02 15:04:05")
			if ev.LastTimestamp.Time.IsZero() {
				timeStr = ev.CreationTimestamp.Time.Format("2006-01-02 15:04:05")
			}

			output.WriteString(fmt.Sprintf("- **[%s] %s** (Count: %d, Last seen: %s)\n", ev.Type, ev.Reason, count, timeStr))
			output.WriteString(fmt.Sprintf("  Object:  %s/%s\n", ev.InvolvedObject.Kind, ev.InvolvedObject.Name))
			output.WriteString(fmt.Sprintf("  Message: %s\n\n", strings.TrimSpace(ev.Message)))
		}

		return mcp.NewToolResultText(output.String()), nil
	})

	if err := server.ServeStdio(mcpServer); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
