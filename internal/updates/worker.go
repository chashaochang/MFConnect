package updates

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 云端宿主机只接收签名发行物，不向业务容器暴露 Docker socket。
type WorkerConfig struct {
	Component    string `json:"component"`
	ControlDir   string `json:"control_dir"`
	RuntimeDir   string `json:"runtime_dir"`
	ComposeDir   string `json:"compose_dir,omitempty"`
	Container    string `json:"container,omitempty"`
	Project      string `json:"project,omitempty"`
	DownloadHost string `json:"download_host,omitempty"`
	DownloadIP   string `json:"download_ip,omitempty"`
}
type Worker struct {
	Config WorkerConfig
	work   string
}

func (w *Worker) path(name string) string { return filepath.Join(w.Config.ControlDir, name) }
func (w *Worker) write(name string, value any) error {
	p := w.path(name)
	if e := Write(p, value); e != nil {
		return e
	}
	if w.Config.Component == "server" && os.Geteuid() == 0 {
		return os.Chown(p, 10001, 10001)
	}
	return nil
}
func (w *Worker) report(j Job, state, code, version string) error {
	return w.write("update-status.json", Report{JobID: j.ID, State: state, Error: code, InstalledVersion: version, UpdatedAt: time.Now().Unix()})
}
func command(ctx context.Context, seconds int, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	// 子命令的输出可能含安装细节，只在内存中处理，不写入公开任务状态。
	cmd := exec.CommandContext(c, args[0], args[1:]...)
	return cmd.Output()
}
func (w *Worker) compose(ctx context.Context, args ...string) error {
	base := []string{"docker", "compose", "--project-directory", w.Config.ComposeDir, "--project-name", w.Config.Project, "-f", filepath.Join(w.Config.ComposeDir, "compose.yaml")}
	_, e := command(ctx, 180, append(base, args...)...)
	return e
}
func (w *Worker) installed(ctx context.Context) string {
	if w.Config.Component == "plugin" {
		b, _ := os.ReadFile("/usr/local/lib/mfconnect/current/VERSION")
		return strings.TrimSpace(string(b))
	}
	b, e := command(ctx, 10, "docker", "exec", w.Config.Container, "mfconnect-server", "version")
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func (w *Worker) ready(ctx context.Context, version string) bool {
	if w.installed(ctx) != version {
		return false
	}
	if w.Config.Component == "plugin" {
		if _, e := os.Stat(filepath.Join(w.Config.ControlDir, "disabled")); e == nil {
			return true
		}
		_, e := command(ctx, 5, "/etc/rc.d/rc.mfconnect", "status")
		var status struct {
			State    string `json:"state"`
			LastSync int64  `json:"last_sync"`
		}
		return e == nil && Read(filepath.Join(w.Config.RuntimeDir, "status.json"), &status) == nil && status.State == "supervising_tunnel" && time.Now().Unix()-status.LastSync < 25
	}
	b, e := command(ctx, 8, "docker", "inspect", "--format", "{{.State.Health.Status}}", w.Config.Container)
	return e == nil && strings.TrimSpace(string(b)) == "healthy"
}
func (w *Worker) waitReady(ctx context.Context, version string) bool {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if w.ready(ctx, version) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}
func Download(ctx context.Context, r Release, path string) error {
	client := &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return downloadWithClient(ctx, r, path, client)
}
func downloadWithClient(ctx context.Context, r Release, path string, client *http.Client) error {
	defer client.CloseIdleConnections()
	req, e := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
	if e != nil {
		return e
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("download_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("download_http_error")
	}
	if resp.ContentLength > 0 && resp.ContentLength != r.Size {
		return errors.New("artifact_size_mismatch")
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, r.Size+1))
	if e != nil {
		return errors.New("download_failed")
	}
	if n != r.Size || hex.EncodeToString(hash.Sum(nil)) != r.SHA256 {
		return errors.New("artifact_checksum_mismatch")
	}
	return f.Sync()
}

// docker load 前检查归档只包含获签名批准的唯一镜像，禁止夹带其他镜像标签。
func ValidateImageArchive(path string, r Release) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	gz, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, 3<<30))
	var manifest []struct {
		Config   string
		RepoTags []string
	}
	configs := map[string]string{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Name == "manifest.json" {
			if h.Size > 1<<20 {
				return errors.New("invalid_image_manifest")
			}
			if json.NewDecoder(tr).Decode(&manifest) != nil {
				return errors.New("invalid_image_manifest")
			}
		}
		if h.Typeflag == tar.TypeReg && h.Size < 1<<20 && (strings.HasSuffix(h.Name, ".json") || strings.HasPrefix(h.Name, "blobs/sha256/")) {
			hash := sha256.New()
			if _, e := io.Copy(hash, tr); e != nil {
				return e
			}
			configs[h.Name] = "sha256:" + hex.EncodeToString(hash.Sum(nil))
		}
	}
	if len(manifest) != 1 || len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != r.Image || configs[manifest[0].Config] != r.ImageID {
		return errors.New("image_identity_mismatch")
	}
	return nil
}

func (w *Worker) backup(ctx context.Context, j Job) error {
	if e := os.MkdirAll(w.work, 0700); e != nil {
		return e
	}
	if w.Config.Component == "plugin" {
		previous, e := filepath.EvalSymlinks("/usr/local/lib/mfconnect/current")
		if e != nil {
			return e
		}
		if !strings.HasPrefix(previous, "/usr/local/lib/mfconnect/versions/") {
			return errors.New("invalid_previous_installation")
		}
		if e := Write(filepath.Join(w.work, "previous.json"), map[string]string{"path": previous}); e != nil {
			return e
		}
		return copyPrivate("/boot/config/plugins/mfconnect.plg", filepath.Join(w.work, "previous.plg"))
	}
	if e := copyPrivate(filepath.Join(w.Config.ComposeDir, ".env"), filepath.Join(w.work, "previous.env")); e != nil {
		return e
	}
	// 在停止容器后备份整个数据目录，避免 SQLite WAL 与主文件不一致。
	if e := w.compose(ctx, "stop", "mfconnect"); e != nil {
		return e
	}
	_, e := command(ctx, 90, "docker", "cp", w.Config.Container+":/var/lib/mfconnect", filepath.Join(w.work, "data"))
	return e
}
func copyPrivate(src, dst string) error {
	b, e := os.ReadFile(src)
	if e != nil {
		return e
	}
	return os.WriteFile(dst, b, 0600)
}
func (w *Worker) install(ctx context.Context, r Release, artifact string) error {
	if w.Config.Component == "plugin" {
		_, e := command(ctx, 300, "/usr/local/sbin/plugin", "install", artifact)
		return e
	}
	b, e := command(ctx, 180, "docker", "load", "--input", artifact)
	_ = b
	if e != nil {
		return e
	}
	b, e = command(ctx, 10, "docker", "image", "inspect", "--format", "{{.Id}}", r.Image)
	if e != nil || strings.TrimSpace(string(b)) != r.ImageID {
		return errors.New("image_identity_mismatch")
	}
	envPath := filepath.Join(w.Config.ComposeDir, ".env")
	raw, e := os.ReadFile(envPath)
	if e != nil {
		return e
	}
	lines := []string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "MF_IMAGE_TAG=") {
			lines = append(lines, l)
		}
	}
	lines = append(lines, "MF_IMAGE_TAG="+strings.TrimPrefix(r.Image, "mfconnect-server:"))
	if e = os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0600); e != nil {
		return e
	}
	return w.compose(ctx, "up", "-d", "--no-build", "--pull", "never", "mfconnect")
}
func (w *Worker) rollback(ctx context.Context, j Job) bool {
	if e := w.report(j, "rolling_back", "", ""); e != nil {
		return false
	}
	if w.Config.Component == "plugin" {
		var old map[string]string
		if Read(filepath.Join(w.work, "previous.json"), &old) != nil {
			return false
		}
		if !strings.HasPrefix(old["path"], "/usr/local/lib/mfconnect/versions/") {
			return false
		}
		if _, e := command(ctx, 60, "bash", filepath.Join(old["path"], "install.sh")); e != nil {
			return false
		}
		if e := copyPrivate(filepath.Join(w.work, "previous.plg"), "/boot/config/plugins/mfconnect.plg"); e != nil {
			return false
		}
	} else {
		if e := copyPrivate(filepath.Join(w.work, "previous.env"), filepath.Join(w.Config.ComposeDir, ".env")); e != nil {
			return false
		}
		if e := w.compose(ctx, "up", "-d", "--no-build", "--pull", "never", "mfconnect"); e != nil {
			return false
		}
	}
	return w.waitReady(ctx, j.PreviousVersion)
}
func (w *Worker) perform(ctx context.Context, j Job) {
	// 执行器退出不是安装失败；保留持久阶段，让下一次启动继续核验。
	if ctx.Err() != nil {
		return
	}
	r, e := j.Release.Verify(PublicKey, time.Now())
	if e != nil || r.Component != w.Config.Component || j.ReleaseID != r.ID || j.Version != r.Version || !JobIDPattern.MatchString(j.ID) {
		_ = w.report(j, "failed", "invalid_release", "")
		return
	}
	w.work = filepath.Join(w.Config.ControlDir, "backups", j.ID)
	var previous Report
	_ = Read(w.path("update-status.json"), &previous)
	if previous.JobID == j.ID && Terminal(previous.State) {
		return
	}
	if previous.JobID == j.ID && (previous.State == "installing" || previous.State == "verifying" || previous.State == "rolling_back") {
		if w.waitReady(ctx, j.Version) {
			_ = w.report(j, "succeeded", "", j.Version)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if w.rollback(ctx, j) {
			_ = w.report(j, "rolled_back", "update_interrupted", j.PreviousVersion)
		} else if ctx.Err() == nil {
			_ = w.report(j, "failed", "rollback_failed", w.installed(ctx))
		}
		return
	}
	current := w.installed(ctx)
	if j.ExpiresAt < time.Now().Unix() {
		_ = w.report(j, "failed", "job_expired", current)
		return
	}
	if !ValidVersion(current) || current != j.PreviousVersion || Compare(j.Version, current) <= 0 {
		_ = w.report(j, "failed", "installed_version_changed", current)
		return
	}
	if e = w.report(j, "downloading", "", current); e != nil {
		return
	}
	temp, e := os.MkdirTemp(w.Config.RuntimeDir, "artifact-")
	if e != nil {
		_ = w.report(j, "failed", "storage_unavailable", current)
		return
	}
	defer os.RemoveAll(temp)
	name := "image.tar.gz"
	if r.Component == "plugin" {
		name = "mfconnect.plg"
	}
	artifact := filepath.Join(temp, name)
	client := &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if w.Config.DownloadHost != "" && net.ParseIP(w.Config.DownloadIP) != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		dialer := net.Dialer{Timeout: 15 * time.Second}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, e := net.SplitHostPort(address)
			if e == nil && host == w.Config.DownloadHost {
				address = net.JoinHostPort(w.Config.DownloadIP, port)
			}
			return dialer.DialContext(ctx, network, address)
		}
		client.Transport = transport
	}
	if e = downloadWithClient(ctx, r, artifact, client); e != nil {
		if ctx.Err() == nil {
			_ = w.report(j, "failed", "download_or_checksum_failed", current)
		}
		return
	}
	if r.Component == "server" {
		if e = ValidateImageArchive(artifact, r); e != nil {
			_ = w.report(j, "failed", "image_identity_mismatch", current)
			return
		}
	}
	// 先持久化阶段再更换进程；宿主机/插件重启后据此恢复或回退。
	if e = w.report(j, "installing", "", current); e != nil {
		return
	}
	if e = w.backup(ctx, j); e == nil {
		e = w.install(ctx, r, artifact)
	}
	if e == nil {
		e = w.report(j, "verifying", "", "")
		if e == nil && w.waitReady(ctx, j.Version) {
			_ = w.report(j, "succeeded", "", j.Version)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	if w.rollback(ctx, j) {
		_ = w.report(j, "rolled_back", "installation_or_healthcheck_failed", j.PreviousVersion)
	} else if ctx.Err() == nil {
		_ = w.report(j, "failed", "rollback_failed", w.installed(ctx))
	}
}
func (w *Worker) Run(ctx context.Context) error {
	if w.Config.Component != "plugin" && w.Config.Component != "server" {
		return errors.New("invalid worker component")
	}
	if w.Config.Component == "server" && (w.Config.Project != "mfconnect" || w.Config.Container != "mfconnect-mfconnect-1" || !filepath.IsAbs(w.Config.ComposeDir)) {
		return errors.New("invalid cloud worker configuration")
	}
	for _, p := range []string{w.Config.ControlDir, w.Config.RuntimeDir} {
		if !filepath.IsAbs(p) {
			return errors.New("absolute paths required")
		}
		if e := os.MkdirAll(p, 0700); e != nil {
			return e
		}
	}
	lock, e := os.OpenFile(filepath.Join(w.Config.RuntimeDir, "updater.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("updater already running")
	}
	beat := func() {
		_ = w.write("update-heartbeat.json", Heartbeat{Protocol: Protocol, UpdatedAt: time.Now().Unix()})
	}
	// 插件心跳写入 RAM，避免每几秒磨损 Unraid 启动盘。
	if w.Config.Component == "plugin" {
		beat = func() {
			_ = Write(filepath.Join(w.Config.RuntimeDir, "update-heartbeat.json"), Heartbeat{Protocol: Protocol, UpdatedAt: time.Now().Unix()})
		}
	}
	beat()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				beat()
			}
		}
	}()
	for {
		var j Job
		if Read(w.path("update-request.json"), &j) == nil {
			var r Report
			_ = Read(w.path("update-status.json"), &r)
			if r.JobID != j.ID || !Terminal(r.State) {
				w.perform(ctx, j)
			}
		}
		// 安装器不能中断当前执行器；任务落盘后再切换到新包内的执行器。
		if w.Config.Component == "plugin" {
			current, e := filepath.EvalSymlinks("/usr/local/lib/mfconnect/current/mfconnect-updater")
			exe, _ := os.Executable()
			if e == nil && current != exe {
				var r Report
				if Read(w.path("update-status.json"), &r) == nil && Terminal(r.State) {
					return syscall.Exec(current, []string{current, "--plugin"}, os.Environ())
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func PluginSoftware(configPath string) (*Software, *Report) {
	exe, e := os.Executable()
	if e != nil {
		return nil, nil
	}
	dir := filepath.Dir(exe)
	b, e := os.ReadFile(filepath.Join(dir, "VERSION"))
	if e != nil {
		return nil, nil
	}
	version := strings.TrimSpace(string(b))
	if !ValidVersion(version) {
		return nil, nil
	}
	var beat Heartbeat
	protocol := 0
	if Read("/var/run/mfconnect/update-heartbeat.json", &beat) == nil && time.Now().Unix()-beat.UpdatedAt < 35 {
		protocol = beat.Protocol
	}
	info := &Software{Plugin: version, UpdateProtocol: protocol}
	var report Report
	if Read(filepath.Join(filepath.Dir(configPath), "update-status.json"), &report) != nil || !ValidReport(report) {
		return info, nil
	}
	return info, &report
}
func AcceptPluginJob(configPath string, j *Job) error {
	if j == nil {
		return nil
	}
	r, e := j.Release.Verify(PublicKey, time.Now())
	if e != nil || j.Component != "plugin" || r.Component != "plugin" || j.Version != r.Version || j.ReleaseID != r.ID || !JobIDPattern.MatchString(j.ID) {
		return errors.New("invalid update job")
	}
	path := filepath.Join(filepath.Dir(configPath), "update-request.json")
	var old Job
	if Read(path, &old) == nil && old.ID == j.ID {
		return nil
	}
	return Write(path, j)
}
