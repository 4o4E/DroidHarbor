package platform

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"droidharbor/internal/transfers"
)

func (s *Server) transferAdmin(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	owner, ok := s.mediaAdmin(w, r, write)
	if !ok {
		return "", false
	}
	if s.Transfers == nil {
		s.fail(w, r, 503, "TRANSFERS_UNAVAILABLE", "文件服务暂不可用")
		return "", false
	}
	return owner, true
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.transferAdmin(w, r, true)
	if !ok {
		return
	}
	var body transfers.CreateRequest
	if !s.decode(w, r, &body, false) {
		return
	}
	value, err := s.Transfers.Create(r.Context(), owner, r.PathValue("id"), body)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, http.StatusCreated, value)
}

// The 60-second idle bound advances only when the client supplies more bytes;
// it is not a throughput requirement or an absolute large-file timeout.
type uploadBody struct {
	io.ReadCloser
	controller *http.ResponseController
	mu         sync.Mutex
	closed     bool
}

func (body *uploadBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	if body.closed {
		body.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	err := body.controller.SetReadDeadline(time.Now().Add(time.Minute))
	body.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return body.ReadCloser.Read(buffer)
}
func (body *uploadBody) Close() error {
	body.mu.Lock()
	body.closed = true
	deadlineErr := body.controller.SetReadDeadline(time.Now())
	body.mu.Unlock()
	return errors.Join(deadlineErr, body.ReadCloser.Close())
}

func (s *Server) uploadTransfer(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.transferAdmin(w, r, true)
	if !ok {
		return
	}
	if r.ContentLength < 0 {
		s.fail(w, r, 411, "CONTENT_LENGTH_REQUIRED", "上传必须声明完整字节长度")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/octet-stream" || (r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity") {
		s.fail(w, r, 415, "UNSUPPORTED_UPLOAD_ENCODING", "上传正文必须为未压缩的 application/octet-stream 原始文件字节")
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		s.fail(w, r, 503, "UPLOAD_UNAVAILABLE", "无法限定上传连接的闲置等待时间")
		return
	}
	defer func() {
		if err := controller.SetReadDeadline(time.Time{}); err != nil {
			s.Logger.Info("upload_deadline_reset_failed", "requestId", r.Context().Value(requestKey{}))
		}
	}()
	value, err := s.Transfers.Upload(r.Context(), owner, r.PathValue("id"), r.ContentLength, &uploadBody{ReadCloser: r.Body, controller: controller})
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	// Staging completion is not installation or device-file completion.
	s.json(w, http.StatusAccepted, value)
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.transferAdmin(w, r, false)
	if !ok {
		return
	}
	value, err := s.Transfers.Get(r.Context(), owner, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, value)
}

func (s *Server) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.transferAdmin(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.Transfers.Cancel(ctx, owner, r.PathValue("id")); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
