package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"
)

type apiClient struct {
	base   *url.URL
	client *http.Client
}

func parseHTTPSOrigin(origin string) (*url.URL, *commandFailure) {
	base, err := url.Parse(origin)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, &commandFailure{Code: "INVALID_ARGUMENT", Message: "平台地址必须为 HTTPS origin，不能包含用户信息、查询参数、片段或路径前缀", exitCode: 2}
	}
	base.Path = ""
	return base, nil
}

func newAPI(origin, caPath string) (*apiClient, *commandFailure) {
	base, failure := parseHTTPSOrigin(origin)
	if failure != nil {
		return nil, failure
	}
	var err error
	var roots *x509.CertPool
	if caPath != "" {
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, &commandFailure{Code: "TLS_CONFIGURATION_ERROR", Message: "无法读取系统信任证书", exitCode: 2}
		}
		pem, readErr := os.ReadFile(caPath)
		if readErr != nil {
			return nil, &commandFailure{Code: "TLS_CONFIGURATION_ERROR", Message: readErr.Error(), exitCode: 2}
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, &commandFailure{Code: "TLS_CONFIGURATION_ERROR", Message: "CA 文件不包含有效 PEM 证书", exitCode: 2}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &apiClient{base: base, client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (a *apiClient) do(method, path string, body io.Reader, headers map[string]string) ([]byte, int, string, error) {
	u := *a.base
	endpoint, err := url.Parse("/api/v1" + path)
	if err != nil {
		return nil, 0, "", err
	}
	u.Path, u.RawPath = endpoint.Path, endpoint.RawPath
	request, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, 0, "", err
	}
	// A consumed lease request body cannot be replayed, including when the
	// claim's Idempotency-Key would otherwise make POST eligible for replay.
	request.GetBody = nil
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, 0, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	closeErr := response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	if readErr != nil || closeErr != nil || len(data) > 65536 {
		return nil, response.StatusCode, requestID, errors.Join(readErr, closeErr, errors.New("平台响应不完整或超过 64 KiB"))
	}
	return data, response.StatusCode, requestID, nil
}

func httpFailure(data []byte, status int, requestID string) *commandFailure {
	f := &commandFailure{exitCode: 4, RequestID: requestID}
	if json.Unmarshal(data, f) != nil || f.Code == "" || f.Message == "" {
		f.Code = "HTTP_ERROR"
		f.Message = fmt.Sprintf("平台返回 HTTP %d；未跟随重定向", status)
		f.RequestID = requestID
	} else if f.RequestID == "" {
		f.RequestID = requestID
	}
	switch status {
	case 401, 403:
		f.exitCode = 3
	case 404, 409, 410, 422:
		f.exitCode = 5
	}
	return f
}

func readCredential(path string) (string, *commandFailure) {
	file, err := os.Open(path)
	if err != nil {
		return "", &commandFailure{Code: "CREDENTIAL_FILE_ERROR", Message: err.Error(), exitCode: 2}
	}
	info, err := file.Stat()
	if err != nil {
		return "", &commandFailure{Code: "CREDENTIAL_FILE_ERROR", Message: errors.Join(err, file.Close()).Error(), exitCode: 2}
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		message := "凭证文件不能允许其他用户或用户组读写；请设置为 0600"
		if closeErr := file.Close(); closeErr != nil {
			message = errors.Join(errors.New(message), closeErr).Error()
		}
		return "", &commandFailure{Code: "CREDENTIAL_FILE_PERMISSIONS", Message: message, exitCode: 2}
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 16385))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 16384 {
		message := errors.Join(readErr, closeErr, errors.New("无法完整读取凭证文件或文件超过 16 KiB")).Error()
		return "", &commandFailure{Code: "CREDENTIAL_FILE_ERROR", Message: message, exitCode: 2}
	}
	var credential struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(data, &credential) != nil || credential.Token == "" {
		return "", &commandFailure{Code: "INVALID_CREDENTIAL_FILE", Message: "凭证文件必须为包含非空 token 的 JSON 对象", exitCode: 2}
	}
	return credential.Token, nil
}
