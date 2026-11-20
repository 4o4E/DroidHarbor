package transfers

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/diagnostics"
)

const (
	shellPacketStdin      byte = 0
	shellPacketStdout     byte = 1
	shellPacketStderr     byte = 2
	shellPacketExit       byte = 3
	shellPacketCloseStdin byte = 4
	shellWriteChunk            = 64 * 1024
)

type commandResult struct {
	Stdout   string
	Stderr   string
	ExitCode uint8
}

func (m *Manager) Upload(ctx context.Context, owner, transferID string, size int64, src io.Reader) (Transfer, error) {
	if src == nil {
		return Transfer{}, problem(400, "TRANSFER_CONTENT_REQUIRED", "上传内容不能为空", nil)
	}
	if size < 0 || size > m.config.MaxUploadBytes {
		return Transfer{}, problem(413, "TRANSFER_SIZE_LIMIT", "文件大小不符合传输限制", nil)
	}
	t, err := m.authorizedTransfer(ctx, owner, transferID)
	if err != nil {
		return Transfer{}, err
	}
	m.mu.Lock()
	if t.closed || t.value.State != StateUploading {
		m.mu.Unlock()
		return Transfer{}, problem(409, "TRANSFER_NOT_UPLOADABLE", "该传输不再接收文件内容", nil)
	}
	if t.claimed {
		m.mu.Unlock()
		return Transfer{}, problem(409, "TRANSFER_CONTENT_ALREADY_SENT", "该传输内容已经提交", nil)
	}
	if size != t.value.Size {
		m.mu.Unlock()
		return Transfer{}, problem(422, "TRANSFER_SIZE_MISMATCH", "上传长度与创建时声明不一致", nil)
	}
	t.claimed = true
	m.mu.Unlock()
	defer close(t.uploadDone)

	uploadCtx, cancel := context.WithCancel(ctx)
	stopScope := context.AfterFunc(t.ctx, cancel)
	defer func() { stopScope(); cancel() }()
	readerCloseDone := make(chan struct{})
	stopReader := context.AfterFunc(uploadCtx, func() {
		if closer, ok := src.(io.Closer); ok {
			_ = closer.Close()
		}
		close(readerCloseDone)
	})
	defer func() {
		if !stopReader() {
			<-readerCloseDone
		}
	}()
	file, err := os.OpenFile(t.path, os.O_WRONLY, 0600)
	if err != nil {
		return m.failUpload(t, "STAGING_OPEN_FAILED", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = errors.New("transfer staging path is not a regular file")
		}
		return m.failUpload(t, "STAGING_INVALID", err)
	}
	var total int64
	var chunk [shellWriteChunk]byte
	for total < size {
		if err := uploadCtx.Err(); err != nil {
			_ = file.Close()
			return m.failUpload(t, "UPLOAD_CANCELLED", err)
		}
		if err := t.scope.Check(); err != nil {
			_ = file.Close()
			return m.failUpload(t, "SESSION_UNAVAILABLE", err)
		}
		want := int64(len(chunk))
		if remaining := size - total; remaining < want {
			want = remaining
		}
		n, readErr := io.ReadFull(src, chunk[:int(want)])
		if n > 0 {
			if err := t.scope.Check(); err != nil {
				_ = file.Close()
				return m.failUpload(t, "SESSION_UNAVAILABLE", err)
			}
			if err := writeLocalAll(file, chunk[:n], func(written int) { total += int64(written); m.update(t, func(v *Transfer) { v.BytesReceived = total }) }); err != nil {
				_ = file.Close()
				return m.failUpload(t, "STAGING_WRITE_FAILED", err)
			}
		}
		if readErr != nil {
			_ = file.Close()
			if cancelErr := uploadCtx.Err(); cancelErr != nil {
				return m.failUpload(t, "UPLOAD_CANCELLED", cancelErr)
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return m.failUpload(t, "UPLOAD_SIZE_MISMATCH", io.ErrUnexpectedEOF)
			}
			return m.failUpload(t, "UPLOAD_READ_FAILED", readErr)
		}
	}
	if err := t.scope.Check(); err != nil {
		_ = file.Close()
		return m.failUpload(t, "SESSION_UNAVAILABLE", err)
	}
	var extra [1]byte
	extraN, extraErr := io.ReadFull(src, extra[:])
	if extraN != 0 {
		_ = file.Close()
		return m.failUpload(t, "UPLOAD_SIZE_MISMATCH", errors.New("upload exceeds declared content length"))
	}
	if !errors.Is(extraErr, io.EOF) {
		_ = file.Close()
		if extraErr == nil {
			extraErr = errors.New("upload has trailing content")
		}
		return m.failUpload(t, "UPLOAD_READ_FAILED", extraErr)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return m.failUpload(t, "STAGING_SYNC_FAILED", err)
	}
	if err := file.Close(); err != nil {
		return m.failUpload(t, "STAGING_CLOSE_FAILED", err)
	}
	if err := uploadCtx.Err(); err != nil {
		return m.failUpload(t, "UPLOAD_CANCELLED", err)
	}
	updated, err := m.db.ExecContext(uploadCtx, `UPDATE owned_resources SET state='active' WHERE id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND locator=? AND state='pending'`, t.value.ID, localOwnerKind, t.sessionID, m.config.BootID, Kind, t.locator)
	if err != nil {
		return m.failUpload(t, "STAGING_JOURNAL_FAILED", err)
	}
	if affected, err := updated.RowsAffected(); err != nil || affected != 1 {
		if err == nil {
			err = errors.New("transfer staging journal changed")
		}
		return m.failUpload(t, "STAGING_JOURNAL_FAILED", err)
	}
	if err := t.scope.Check(); err != nil {
		return m.failUpload(t, "SESSION_UNAVAILABLE", err)
	}
	m.mu.Lock()
	if m.closed || t.ctx.Err() != nil {
		m.mu.Unlock()
		return m.failUpload(t, "SESSION_ENDED", context.Canceled)
	}
	t.started = true
	t.value.State = StateTransferring
	t.value.UpdatedAt = time.Now().UTC()
	m.workers.Add(1)
	m.mu.Unlock()
	if err := m.setStatus(t, StateTransferring, "", ""); err != nil {
		m.workers.Done()
		_ = m.finish(t, StateFailed, "RESULT_JOURNAL_FAILED", "无法记录传输状态", err.Error(), nil)
		return m.snapshot(t), problem(503, "RESULT_JOURNAL_FAILED", "无法记录传输状态", err)
	}
	go m.run(t)
	return m.snapshot(t), nil
}

func (m *Manager) failUpload(t *transfer, code string, cause error) (Transfer, error) {
	m.logOperationFailure(t, code, cause)
	state := StateFailed
	status := 500
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		state = StateCancelled
		status = 410
	}
	if code == "UPLOAD_SIZE_MISMATCH" || code == "UPLOAD_READ_FAILED" {
		status = 400
	}
	message := code
	if state == StateCancelled {
		message = "上传已取消"
	}
	_ = m.finish(t, state, code, message, "", nil)
	return m.snapshot(t), problem(status, code, message, cause)
}

func writeLocalAll(w io.Writer, data []byte, progress func(int)) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("invalid local staging write count")
		}
		data = data[n:]
		if n > 0 && progress != nil {
			progress(n)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type finishOutcome struct {
	state   string
	code    string
	message string
	result  string
	exit    *uint8
}

func (m *Manager) run(t *transfer) {
	defer m.workers.Done()
	outcome := m.execute(t)
	_ = m.finish(t, outcome.state, outcome.code, outcome.message, outcome.result, outcome.exit)
}

func (m *Manager) execute(t *transfer) finishOutcome {
	ctx := t.ctx
	if err := ctx.Err(); err != nil {
		m.logOperationFailure(t, "DEVICE_OPERATION_NOT_STARTED", err)
		return finishOutcome{StateCancelled, "DEVICE_OPERATION_NOT_STARTED", "设备操作尚未开始", "", nil}
	}
	if err := t.scope.Check(); err != nil {
		m.logOperationFailure(t, "DEVICE_OPERATION_NOT_AUTHORIZED", err)
		return finishOutcome{StateFailed, "DEVICE_OPERATION_NOT_AUTHORIZED", "设备操作未授权", "", nil}
	}
	file, err := os.Open(t.path)
	if err != nil {
		m.logOperationFailure(t, "STAGING_OPEN_FAILED", err)
		return finishOutcome{StateFailed, "STAGING_OPEN_FAILED", "无法读取受限临时文件", "", nil}
	}
	defer func() {
		if err := file.Close(); err != nil {
			m.logOperationFailure(t, "STAGING_CLOSE_FAILED", err)
		}
	}()
	command := ""
	if t.value.Purpose == "file" {
		command = fileWriteCommand(t.value.TargetPath, t.value.Size, t.value.Overwrite, t.ifMatch)
	} else {
		if err := m.setStatus(t, StateInstalling, "", ""); err != nil {
			m.logOperationFailure(t, "RESULT_JOURNAL_FAILED", err)
			return finishOutcome{StateFailed, "RESULT_JOURNAL_FAILED", "无法记录安装操作状态", "", nil}
		}
		if err := t.scope.Check(); err != nil {
			m.logOperationFailure(t, "DEVICE_OPERATION_NOT_AUTHORIZED", err)
			return finishOutcome{StateFailed, "DEVICE_OPERATION_NOT_AUTHORIZED", "安装操作未授权", "", nil}
		}
		// With -S and no path, Package Manager consumes the APK from stdin.
		command = "pm install -r --user current -S " + fmt.Sprint(t.value.Size)
	}
	if err := t.scope.Check(); err != nil {
		m.logOperationFailure(t, "DEVICE_OPERATION_NOT_AUTHORIZED", err)
		return finishOutcome{StateFailed, "DEVICE_OPERATION_NOT_AUTHORIZED", "设备操作未授权", "", nil}
	}
	result, err := streamShell(ctx, m.adb, t.scope.Link.TransportID, command, file, t.value.Size, m.config.MaxCommandOutput, t.scope.Check, func(n int64) {
		m.update(t, func(v *Transfer) { v.BytesTransferred = n })
	})
	m.update(t, func(v *Transfer) { v.Stdout = result.Stdout; v.Stderr = result.Stderr })
	if err != nil {
		m.logOperationFailure(t, "DEVICE_STREAM_FAILED", err)
		code := "DEVICE_RESULT_UNKNOWN"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code = "TRANSFER_CANCELLED_RESULT_UNKNOWN"
		}
		return finishOutcome{StateUnknown, code, "设备端结果未知", "", nil}
	}
	if t.value.Purpose == "apk" {
		out := strings.TrimSpace(result.Stdout)
		if result.ExitCode == 0 && (out == "Success" || strings.HasPrefix(out, "Success:")) {
			if err := t.scope.Check(); err != nil {
				m.logOperationFailure(t, "DEVICE_OPERATION_NOT_AUTHORIZED", err)
				return finishOutcome{StateUnknown, "DEVICE_RESULT_UNKNOWN", "安装结果未知", shellResultText(result), &result.ExitCode}
			}
			return finishOutcome{StateSucceeded, "", "", shellResultText(result), &result.ExitCode}
		}
		if result.ExitCode != 0 || strings.Contains(out, "Failure [") || strings.Contains(out, "Failure:") {
			return finishOutcome{StateFailed, "PACKAGE_MANAGER_FAILED", "Package Manager 拒绝安装", shellResultText(result), &result.ExitCode}
		}
		return finishOutcome{StateUnknown, "PACKAGE_MANAGER_RESULT_UNKNOWN", "Package Manager 未返回可确认的结果", shellResultText(result), &result.ExitCode}
	}
	if result.ExitCode == 73 {
		return finishOutcome{StateFailed, "DEVICE_FILE_EXISTS", "设备已存在同名项目，未覆盖", "", &result.ExitCode}
	}
	if result.ExitCode == 75 {
		return finishOutcome{StateFailed, "DEVICE_FILE_CHANGED", "手机上的原文件已变化，未覆盖，请核对后重新编辑", "", &result.ExitCode}
	}
	if result.ExitCode == 66 || result.ExitCode == 67 {
		return finishOutcome{StateFailed, "DEVICE_DIRECTORY_FORBIDDEN", "目标目录不存在或不在共享存储内，未写入", "", &result.ExitCode}
	}
	if result.ExitCode != 0 {
		return finishOutcome{StateFailed, "DEVICE_WRITE_FAILED", "设备未能写入目标文件", shellResultText(result), &result.ExitCode}
	}
	if err := t.scope.Check(); err != nil {
		m.logOperationFailure(t, "DEVICE_OPERATION_NOT_AUTHORIZED", err)
		return finishOutcome{StateUnknown, "DEVICE_RESULT_UNKNOWN", "设备端结果未知", "", &result.ExitCode}
	}
	return finishOutcome{StateSucceeded, "", "", "", &result.ExitCode}
}

func fileWriteCommand(target string, size int64, overwrite bool, match string) string {
	// Work from the verified physical cwd, not through an earlier path alias.
	dir, _ := adb.ShellQuote(path.Dir(target))
	name, _ := adb.ShellQuote("./" + path.Base(target))
	setup := adb.SharedDirectorySetup + "cd -P " + dir + " 2>/dev/null || exit 66\n" + adb.SharedUserDirectoryCheck + "file=" + name + "\numask 022\n"
	const noTarget = `[ ! -e "$file" ] && [ ! -L "$file" ] || exit 73
`
	const regularTarget = `if [ -L "$file" ] || { [ -e "$file" ] && [ ! -f "$file" ]; }; then exit 73; fi
`
	// A closed ADB stream can look like EOF to cat. Publish only the declared
	// byte count, and keep incomplete bytes out of the destination name.
	stage := `tmp=$(mktemp ./.droidharbor-upload.XXXXXXXX) || exit 1
trap 'rm -f -- "$tmp" || exit 1' EXIT
trap 'exit 1' HUP INT TERM
cat > "$tmp" && chmod 0644 "$tmp" || exit 1
[ "$(stat -c %s -- "$tmp")" = ` + fmt.Sprint(size) + ` ] || exit 1
`
	if !overwrite {
		return setup + noTarget + stage + `mv -nT -- "$tmp" "$file" || exit 1
[ ! -e "$tmp" ] || exit 73
`
	}
	return setup + regularTarget + stage + regularTarget + fileVersionCheck(match) + `mv -fT -- "$tmp" "$file"
`
}

func fileVersionCheck(match string) string {
	if match == "" {
		return ""
	}
	return `[ ! -L "$file" ] && [ -f "$file" ] || exit 75
digest=$(sha256sum < "$file") || exit 75
[ "${digest%% *}" = '` + match + `' ] || exit 75
`
}

func (m *Manager) logOperationFailure(t *transfer, code string, cause error) {
	m.logger.Warn("transfer_operation_failed", "transferId", t.value.ID, "code", code, "cause", diagnostics.Cause(cause))
}

func incompleteShellResult(result commandResult, readErr error, zeroExitMessage string) (commandResult, error) {
	if readErr != nil {
		return result, readErr
	}
	if result.ExitCode != 0 {
		return result, nil
	}
	return result, errors.New(zeroExitMessage)
}

func streamShell(ctx context.Context, client *adb.Client, transportID uint64, command string, source io.Reader, size, maxOutput int64, authorize func() error, progress func(int64)) (commandResult, error) {
	var empty commandResult
	conn, err := client.Open(ctx, transportID, "shell,v2,raw:"+command, authorize)
	if err != nil {
		return empty, err
	}
	defer conn.Close()
	type readResult struct {
		value commandResult
		err   error
	}
	readDone := make(chan readResult, 1)
	go func() { value, err := readShellResult(conn, maxOutput); readDone <- readResult{value, err} }()
	reconcile := func(completed readResult, cause error) (commandResult, error) {
		if completed.err != nil {
			return completed.value, errors.Join(cause, completed.err)
		}
		if completed.value.ExitCode != 0 {
			return completed.value, nil
		}
		return completed.value, cause
	}
	var sent int64
	var chunk [shellWriteChunk]byte
	for sent < size {
		select {
		case completed := <-readDone:
			return incompleteShellResult(completed.value, completed.err, "shell_v2 exited successfully before input was completed")
		default:
		}
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			completed := <-readDone
			return reconcile(completed, err)
		}
		if err := authorize(); err != nil {
			_ = conn.Close()
			completed := <-readDone
			return reconcile(completed, err)
		}
		want := int64(len(chunk))
		if remaining := size - sent; remaining < want {
			want = remaining
		}
		n, readErr := io.ReadFull(source, chunk[:int(want)])
		if readErr != nil {
			_ = conn.Close()
			completed := <-readDone
			return reconcile(completed, fmt.Errorf("read transfer staging content: %w", readErr))
		}
		if err := writeShellStdin(conn, chunk[:n], func(written int) {
			sent += int64(written)
			if progress != nil {
				progress(sent)
			}
		}); err != nil {
			_ = conn.Close()
			completed := <-readDone
			return reconcile(completed, err)
		}
	}
	select {
	case completed := <-readDone:
		return incompleteShellResult(completed.value, completed.err, "shell_v2 exited successfully before stdin was closed")
	default:
	}
	if err := writeShellPacket(conn, shellPacketCloseStdin, nil); err != nil {
		_ = conn.Close()
		completed := <-readDone
		return reconcile(completed, err)
	}
	select {
	case completed := <-readDone:
		return completed.value, completed.err
	case <-ctx.Done():
		_ = conn.Close()
		completed := <-readDone
		return reconcile(completed, ctx.Err())
	}
}

func writeShellStdin(w io.Writer, data []byte, progress func(int)) error {
	for len(data) > 0 {
		n := len(data)
		if n > shellWriteChunk {
			n = shellWriteChunk
		}
		if err := writeShellPacketProgress(w, shellPacketStdin, data[:n], progress); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
func writeShellPacket(w io.Writer, id byte, data []byte) error {
	return writeShellPacketProgress(w, id, data, nil)
}
func writeShellPacketProgress(w io.Writer, id byte, data []byte, progress func(int)) error {
	var header [5]byte
	header[0] = id
	binary.LittleEndian.PutUint32(header[1:], uint32(len(data)))
	if err := writeStreamAll(w, header[:]); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("invalid ADB shell write count")
		}
		data = data[n:]
		if n > 0 && progress != nil {
			progress(n)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func writeStreamAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("invalid ADB shell write count")
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readShellResult(r io.Reader, maxOutput int64) (commandResult, error) {
	var result commandResult
	var stdout, stderr []byte
	var scratch []byte
	var used int64
	packetLimit := maxOutput
	if packetLimit > 1024*1024 {
		packetLimit = 1024 * 1024
	}
	if packetLimit < 1 {
		packetLimit = 1
	}
	finish := func() { result.Stdout = string(stdout); result.Stderr = string(stderr) }
	for {
		packet, err := adb.ReadShellPacket(r, scratch, uint32(packetLimit))
		if err != nil {
			finish()
			return result, err
		}
		scratch = packet.Data
		if packet.ID == shellPacketExit {
			result.ExitCode = packet.Data[0]
			finish()
			return result, nil
		}
		if packet.ID != shellPacketStdout && packet.ID != shellPacketStderr {
			finish()
			return result, errors.New("unexpected shell_v2 response packet")
		}
		if int64(len(packet.Data)) > maxOutput-used {
			finish()
			return result, errors.New("shell_v2 output exceeds configured limit")
		}
		used += int64(len(packet.Data))
		if packet.ID == shellPacketStdout {
			stdout = append(stdout, packet.Data...)
		} else {
			stderr = append(stderr, packet.Data...)
		}
	}
}

func shellResultText(result commandResult) string {
	if result.Stdout == "" {
		return result.Stderr
	}
	if result.Stderr == "" {
		return result.Stdout
	}
	return result.Stdout + "\n" + result.Stderr
}
