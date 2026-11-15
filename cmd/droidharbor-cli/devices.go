package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
)

type callerDevice struct {
	DeviceID        string   `json:"deviceId"`
	DisplayName     string   `json:"displayName"`
	Tags            []string `json:"tags"`
	ConnectionState string   `json:"connectionState"`
	AllocationState string   `json:"allocationState"`
}

type callerDevices struct {
	Devices        []callerDevice `json:"devices"`
	DiscoveryReady bool           `json:"discoveryReady"`
}

func listDevices(args []string) (callerDevices, *commandFailure) {
	var result callerDevices
	flags := flag.NewFlagSet("devices", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "", "平台 HTTP(S) 地址")
	credential := flags.String("credential-file", "", "含 token 的 JSON 文件")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *server == "" || *credential == "" {
		return result, &commandFailure{Code: "INVALID_ARGUMENT", Message: "用法：droidharbor-cli devices --server <http://host:port> --credential-file <JSON 文件>", exitCode: 2}
	}
	token, failure := readCredential(*credential)
	if failure != nil {
		return result, failure
	}
	api, failure := newAPI(*server)
	if failure != nil {
		return result, failure
	}
	defer api.client.CloseIdleConnections()
	data, status, requestID, err := api.do(http.MethodGet, "/devices", nil, map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return result, &commandFailure{Code: "CONNECTION_FAILED", Message: err.Error(), exitCode: 4}
	}
	if status != http.StatusOK {
		return result, httpFailure(data, status, requestID)
	}
	if json.Unmarshal(data, &result) != nil || result.Devices == nil {
		return callerDevices{}, &commandFailure{Code: "INVALID_RESPONSE", Message: "平台未返回有效的设备列表", RequestID: requestID, exitCode: 4}
	}
	for _, device := range result.Devices {
		if device.DeviceID == "" || device.DisplayName == "" || device.ConnectionState == "" || device.AllocationState == "" {
			return callerDevices{}, &commandFailure{Code: "INVALID_RESPONSE", Message: "设备摘要缺少 ID、名称或状态", RequestID: requestID, exitCode: 4}
		}
	}
	return result, nil
}
