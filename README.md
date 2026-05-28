# Kube-Flux MCP Agent

A Model Context Protocol (MCP) server that empowers AI assistants (like Claude, Cursor, etc.) to seamlessly interact with both your **Local GitOps Repository (Desired State)** and your **Live Kubernetes Cluster (Actual State)**.

Tailored specifically for Kubernetes and FluxCD environments, it allows the AI to debug issues by comparing what is defined in your local YAMLs with what is actually running in the cluster.

## 🚀 Features

The agent exposes two categories of tools to the AI:

### 📂 Local GitOps Tools (Desired State)
Analyzes YAML manifests in your local repository.
* `list_resources` - Lists all Kubernetes/Flux resources found locally, grouped by Kind.
* `get_resource_yaml` - Fetches the raw YAML of a specific local resource.
* `get_helm_values` - Extracts only the `spec.values` from a local Flux `HelmRelease`.

### ☸️ Live Cluster Tools (Actual State)
Connects to your Kubernetes cluster to fetch real-time state. *(Lazy initialized - only connects when requested).*
* `k8s_get_status` - Fetches the live `.status` block of any Kubernetes resource.
* `k8s_get_flux_errors` - Scans live Flux `HelmRelease` and `Kustomization` resources and returns only those that are failing, along with error messages.
* `k8s_get_pod_logs` - Fetches tail logs of a pod by prefix (handles random trailing hashes).
* `k8s_get_events` - Fetches recent Kubernetes events (e.g., Pod crashes, Mount errors), with options to filter by Name, Kind, or Warnings-only.

## 💾 Installation

You don't need to build from source. Pre-compiled binaries for Linux, macOS, and Windows are available on the [Releases page](../../releases).

1. Download the latest release for your OS/Architecture.
2. Make it executable (macOS/Linux): `chmod +x kube-flux-mcp-*`
3. Move it to a directory in your PATH, e.g., `mv kube-flux-mcp-linux-amd64 /usr/local/bin/kube-flux-mcp`

## 🛠️ Usage

### Claude

```json
{
  "mcpServers": {
    "kube-flux-mcp": {
      "command": "/path/to/your/kube-flux-mcp",
      "args": [],
      "env": {
        "PROJECT_ROOT": "/path/to/your/gitops/repository",
        "KUBECONFIG": ".kube/config"
      }
    }
  }
}
```

### Zed-editor

```json
{
	"context_servers": {
		"kube-flux-mcp": {
			"enabled": true,
			"remote": false,
			"command": "/path/to/your/kube-flux-mcp",
			"args": [],
			"env": {
        "PROJECT_ROOT": "/path/to/your/gitops/repository",
        "KUBECONFIG": ".kube/config"
      }
		},
	},
}
```

### Environment Variables

* `PROJECT_ROOT`: Provide the absolute path to your local GitOps repository. *(If omitted, defaults to the current working directory).*
* `KUBECONFIG`: Path to your kubeconfig file. *(If omitted, defaults to `~/.kube/config`).* The agent respects your current context.

## 🏗️ Building from Source

If you prefer to build it yourself, ensure you have Go 1.22+ installed:

```bash
git clone <your-repo>
cd kube-flux-mcp
go mod tidy
go build -o kube-flux-mcp main.go
```
