package plugin_debug

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
	"m7s.live/v5/pb"
	"m7s.live/v5/pkg/util"
)

type EnvCheckResult struct {
	Message string `json:"message"`
	Type    string `json:"type"` // info, success, error, complete
}

// 自定义系统信息响应结构体，用于 JSON 解析
type SysInfoResponseJSON struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
	Data    struct {
		StartTime string `json:"startTime"`
		LocalIP   string `json:"localIP"`
		PublicIP  string `json:"publicIP"`
		Version   string `json:"version"`
		GoVersion string `json:"goVersion"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
		CPUs      int32  `json:"cpus"`
		Plugins   []struct {
			Name        string            `json:"name"`
			PushAddr    []string          `json:"pushAddr"`
			PlayAddr    []string          `json:"playAddr"`
			Description map[string]string `json:"description"`
		} `json:"plugins"`
	} `json:"data"`
}

// 插件配置响应结构体
type PluginConfigResponse struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
	Data    struct {
		File     string `json:"file"`
		Modified string `json:"modified"`
		Merged   string `json:"merged"`
	} `json:"data"`
}

// TCP 配置结构体
type TCPConfig struct {
	ListenAddr    string `yaml:"listenaddr"`
	ListenAddrTLS string `yaml:"listenaddrtls"`
}

// 插件配置结构体
type PluginConfig struct {
	TCP TCPConfig `yaml:"tcp"`
}

func (p *DebugPlugin) EnvCheck(w http.ResponseWriter, r *http.Request) {
	// Get target URL from query parameter
	targetURL := r.URL.Query().Get("target")
	if targetURL == "" {
		r.URL.Path = "/static/envcheck.html"
		staticFSHandler.ServeHTTP(w, r)
		return
	}

	// Create SSE connection
	util.NewSSE(w, r.Context(), func(sse *util.SSE) {
		// Function to send SSE messages
		sendMessage := func(message string, msgType string) {
			result := EnvCheckResult{
				Message: message,
				Type:    msgType,
			}
			sse.WriteJSON(result)
		}

		// Parse target URL
		_, err := url.Parse(targetURL)
		if err != nil {
			sendMessage(fmt.Sprintf("Invalid URL: %v", err), "error")
			return
		}

		// Check if we can connect to the target server
		sendMessage(fmt.Sprintf("Checking connection to %s...", targetURL), "info")

		// Get system info from target server
		resp, err := http.Get(fmt.Sprintf("%s/api/sysinfo", targetURL))
		if err != nil {
			sendMessage(fmt.Sprintf("Failed to connect to target server: %v", err), "error")
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			sendMessage(fmt.Sprintf("Target server returned status code: %d", resp.StatusCode), "error")
			return
		}

		// Read and parse system info
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			sendMessage(fmt.Sprintf("Failed to read response: %v", err), "error")
			return
		}

		var sysInfoJSON SysInfoResponseJSON
		if err := json.Unmarshal(body, &sysInfoJSON); err != nil {
			sendMessage(fmt.Sprintf("Failed to parse system info: %v", err), "error")
			return
		}

		// Convert JSON response to protobuf response
		sysInfo := &pb.SysInfoResponse{
			Code:    sysInfoJSON.Code,
			Message: sysInfoJSON.Message,
			Data: &pb.SysInfoData{
				LocalIP:   sysInfoJSON.Data.LocalIP,
				PublicIP:  sysInfoJSON.Data.PublicIP,
				Version:   sysInfoJSON.Data.Version,
				GoVersion: sysInfoJSON.Data.GoVersion,
				Os:        sysInfoJSON.Data.OS,
				Arch:      sysInfoJSON.Data.Arch,
				Cpus:      sysInfoJSON.Data.CPUs,
			},
		}

		// Parse start time
		if startTime, err := time.Parse(time.RFC3339, sysInfoJSON.Data.StartTime); err == nil {
			sysInfo.Data.StartTime = timestamppb.New(startTime)
		}

		// Convert plugins
		for _, pluginJSON := range sysInfoJSON.Data.Plugins {
			plugin := &pb.PluginInfo{
				Name:        pluginJSON.Name,
				PushAddr:    pluginJSON.PushAddr,
				PlayAddr:    pluginJSON.PlayAddr,
				Description: pluginJSON.Description,
			}
			sysInfo.Data.Plugins = append(sysInfo.Data.Plugins, plugin)
		}

		// Check each plugin's configuration
		for _, plugin := range sysInfo.Data.Plugins {
			// Get plugin configuration
			configResp, err := http.Get(fmt.Sprintf("%s/api/config/get/%s", targetURL, plugin.Name))
			if err != nil {
				sendMessage(fmt.Sprintf("Failed to get configuration for plugin %s: %v", plugin.Name, err), "error")
				continue
			}
			defer configResp.Body.Close()

			if configResp.StatusCode != http.StatusOK {
				sendMessage(fmt.Sprintf("Failed to get configuration for plugin %s: status code %d", plugin.Name, configResp.StatusCode), "error")
				continue
			}

			var configRespJSON PluginConfigResponse
			if err := json.NewDecoder(configResp.Body).Decode(&configRespJSON); err != nil {
				sendMessage(fmt.Sprintf("Failed to parse configuration for plugin %s: %v", plugin.Name, err), "error")
				continue
			}

			// Parse YAML configuration
			var config PluginConfig
			if err := yaml.Unmarshal([]byte(configRespJSON.Data.Merged), &config); err != nil {
				sendMessage(fmt.Sprintf("Failed to parse YAML configuration for plugin %s: %v", plugin.Name, err), "error")
				continue
			}
			// Check TCP configuration
			if config.TCP.ListenAddr != "" {
				host, port, err := net.SplitHostPort(config.TCP.ListenAddr)
				if err != nil {
					sendMessage(fmt.Sprintf("Invalid listenaddr format for plugin %s: %v", plugin.Name, err), "error")
				} else {
					sendMessage(fmt.Sprintf("Checking TCP listenaddr %s for plugin %s...", config.TCP.ListenAddr, plugin.Name), "info")
					// Try to establish TCP connection
					conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 5*time.Second)
					if err != nil {
						sendMessage(fmt.Sprintf("TCP listenaddr %s for plugin %s is not accessible: %v", config.TCP.ListenAddr, plugin.Name, err), "error")
					} else {
						conn.Close()
						sendMessage(fmt.Sprintf("TCP listenaddr %s for plugin %s is accessible", config.TCP.ListenAddr, plugin.Name), "success")
					}
				}
			}

			if config.TCP.ListenAddrTLS != "" {
				host, port, err := net.SplitHostPort(config.TCP.ListenAddrTLS)
				if err != nil {
					sendMessage(fmt.Sprintf("Invalid listenaddrtls format for plugin %s: %v", plugin.Name, err), "error")
				} else {
					sendMessage(fmt.Sprintf("Checking TCP TLS listenaddr %s for plugin %s...", config.TCP.ListenAddrTLS, plugin.Name), "info")
					// Try to establish TCP connection
					conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 5*time.Second)
					if err != nil {
						sendMessage(fmt.Sprintf("TCP TLS listenaddr %s for plugin %s is not accessible: %v", config.TCP.ListenAddrTLS, plugin.Name, err), "error")
					} else {
						conn.Close()
						sendMessage(fmt.Sprintf("TCP TLS listenaddr %s for plugin %s is accessible", config.TCP.ListenAddrTLS, plugin.Name), "success")
					}
				}
			}
		}

		sendMessage("Environment check completed", "complete")
	})
}
