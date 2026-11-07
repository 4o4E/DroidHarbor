package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

type commandFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
	exitCode  int
}

func capabilities(args []string) (json.RawMessage, *commandFailure) {
	flags := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "", "平台 HTTPS origin")
	credential := flags.String("credential-file", "", "含 token 的 JSON 文件")
	ca := flags.String("ca-file", "", "可选的额外 PEM CA 证书")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *server == "" || *credential == "" {
		return nil, &commandFailure{Code: "INVALID_ARGUMENT", Message: "用法：droidharbor-cli capabilities --server <https://host:port> --credential-file <JSON 文件> [--ca-file <PEM 文件>]", exitCode: 2}
	}
	token, failure := readCredential(*credential)
	if failure != nil {
		return nil, failure
	}
	api, failure := newAPI(*server, *ca)
	if failure != nil {
		return nil, failure
	}
	defer api.client.CloseIdleConnections()
	data, status, requestID, err := api.do("GET", "/capabilities", nil, map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return nil, &commandFailure{Code: "CONNECTION_FAILED", Message: err.Error(), exitCode: 4}
	}
	if status != 200 {
		return nil, httpFailure(data, status, requestID)
	}
	var result struct {
		APIMajor int             `json:"apiMajor"`
		Features map[string]bool `json:"features"`
	}
	if json.Unmarshal(data, &result) != nil || result.Features == nil {
		return nil, &commandFailure{Code: "INVALID_RESPONSE", Message: "平台未返回有效的能力对象", exitCode: 4}
	}
	if result.APIMajor != 1 {
		return nil, &commandFailure{Code: "API_INCOMPATIBLE", Message: fmt.Sprintf("CLI 要求 API major 1，平台返回 %d", result.APIMajor), exitCode: 5}
	}
	return json.RawMessage(data), nil
}
