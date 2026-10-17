package updates

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 仅用于运维侧的独立 Docker 项目，普通单元测试不会接触 Docker。
func TestCloudWorkerRollbackIntegration(t *testing.T) {
	path := os.Getenv("MF_UPDATE_ROLLBACK_FIXTURE")
	if path == "" {
		t.Skip("requires isolated cloud Docker fixture")
	}
	var fixture struct {
		Config WorkerConfig `json:"config"`
		Job    Job          `json:"job"`
	}
	if e := Read(path, &fixture); e != nil {
		t.Fatal(e)
	}
	if fixture.Config.Project == "mfconnect" || fixture.Config.Component != "server" {
		t.Fatal("production project forbidden in integration test")
	}
	w := Worker{Config: fixture.Config}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if !w.ready(ctx, fixture.Job.PreviousVersion) {
		t.Fatal("fixture not initially healthy")
	}
	if e := w.report(fixture.Job, "verifying", "", ""); e != nil {
		t.Fatal(e)
	}
	shutdown, cancelShutdown := context.WithCancel(context.Background())
	cancelShutdown()
	w.perform(shutdown, fixture.Job)
	var pending Report
	if Read(filepath.Join(w.Config.ControlDir, "update-status.json"), &pending) != nil || pending.State != "verifying" {
		t.Fatal("executor shutdown overwrote pending recovery result")
	}
	if e := w.report(fixture.Job, "queued", "", fixture.Job.PreviousVersion); e != nil {
		t.Fatal(e)
	}
	w.perform(ctx, fixture.Job)
	var status Report
	if e := Read(filepath.Join(fixture.Config.ControlDir, "update-status.json"), &status); e != nil {
		t.Fatal(e)
	}
	if status.State != "rolled_back" || status.InstalledVersion != fixture.Job.PreviousVersion {
		t.Fatalf("expected verified rollback: %+v", status)
	}
	if !w.ready(ctx, fixture.Job.PreviousVersion) {
		t.Fatal("previous image not restored healthy")
	}
	t.Log("real Docker update health failure rolled back to original image and healthy service")
}
