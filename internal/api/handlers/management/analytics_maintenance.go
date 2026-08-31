package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func (h *Handler) CreateAnalyticsBackup(c *gin.Context) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeAnalyticsJSON(c, &request, 8*1024); err != nil || strings.TrimSpace(request.Path) == "" {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.startAnalyticsJob(c, "backup", map[string]any{"path": request.Path})
}

func (h *Handler) RestoreAnalyticsBackup(c *gin.Context) {
	if strings.TrimSpace(c.Param("id")) == "" {
		writeAnalyticsInvalid(c, fmt.Errorf("backup ID is required"))
		return
	}
	var request struct {
		Path     string `json:"path"`
		Manifest string `json:"manifest"`
	}
	if err := decodeAnalyticsJSON(c, &request, 16*1024); err != nil || strings.TrimSpace(request.Path) == "" || strings.TrimSpace(request.Manifest) == "" {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.startAnalyticsJob(c, "restore", map[string]any{
		"backup_id": c.Param("id"), "path": request.Path, "manifest": request.Manifest,
	})
}

func (h *Handler) ImportCPAUKAnalytics(c *gin.Context) {
	var request struct {
		Path       string `json:"path"`
		BackupPath string `json:"backup_path,omitempty"`
		DryRun     bool   `json:"dry_run"`
		Resume     bool   `json:"resume"`
		BatchID    string `json:"batch_id,omitempty"`
		ChunkSize  int    `json:"chunk_size,omitempty"`
	}
	if err := decodeAnalyticsJSON(c, &request, 16*1024); err != nil || strings.TrimSpace(request.Path) == "" ||
		(!request.DryRun && strings.TrimSpace(request.BackupPath) == "") || request.ChunkSize < 0 || request.ChunkSize > 10_000 {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.startAnalyticsJob(c, "import_cpauk", map[string]any{
		"path": request.Path, "backup_path": request.BackupPath, "dry_run": request.DryRun, "resume": request.Resume,
		"batch_id": request.BatchID, "chunk_size": request.ChunkSize,
	})
}

func (h *Handler) RollbackAnalyticsImport(c *gin.Context) {
	batchID := strings.TrimSpace(c.Param("batch_id"))
	if batchID == "" || len(batchID) > 128 {
		writeAnalyticsInvalid(c, fmt.Errorf("invalid batch ID"))
		return
	}
	h.startAnalyticsJob(c, "rollback_import", map[string]any{"batch_id": batchID})
}

func (h *Handler) PurgeAnalyticsKey(c *gin.Context) {
	var request struct {
		KeyID      string `json:"key_id"`
		Preview    bool   `json:"preview,omitempty"`
		Confirmed  bool   `json:"confirmed"`
		BatchID    string `json:"batch_id,omitempty"`
		BackupPath string `json:"backup_path"`
	}
	if err := decodeAnalyticsJSON(c, &request, 8*1024); err != nil || !model.IsFullKeyID(request.KeyID) ||
		request.Preview && (request.Confirmed || request.BatchID != "" || request.BackupPath != "") ||
		!request.Preview && (!request.Confirmed || !validPurgeBatchID(request.BatchID) || strings.TrimSpace(request.BackupPath) == "") {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.startAnalyticsJob(c, "purge_key", map[string]any{
		"key_id": request.KeyID, "preview": request.Preview, "confirmed": request.Confirmed,
		"batch_id": request.BatchID, "backup_path": request.BackupPath,
	})
}

func validPurgeBatchID(value string) bool {
	if len(value) != len("purge-")+64 || !strings.HasPrefix(value, "purge-") {
		return false
	}
	for _, character := range value[len("purge-"):] {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func (h *Handler) RepairAnalytics(c *gin.Context) {
	var request struct {
		Kind        string `json:"kind"`
		ArchivePath string `json:"archive_path,omitempty"`
		Confirmed   bool   `json:"confirmed,omitempty"`
	}
	if err := decodeAnalyticsJSON(c, &request, 8*1024); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	switch request.Kind {
	case "integrity_check", "checkpoint", "reindex":
		h.startAnalyticsJob(c, request.Kind, nil)
	case "start_new_identity_epoch":
		if !request.Confirmed || strings.TrimSpace(request.ArchivePath) == "" {
			writeAnalyticsInvalid(c, fmt.Errorf("new identity epoch requires confirmation and archive_path"))
			return
		}
		h.startAnalyticsJob(c, request.Kind, map[string]any{"confirmed": true, "archive_path": request.ArchivePath})
	case "retry_intake":
		service := h.analyticsService()
		if service == nil {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
		if err := service.Retry(c.Request.Context()); err != nil {
			writeAnalyticsError(c, err)
			return
		}
		setAnalyticsNoStore(c)
		c.JSON(http.StatusAccepted, gin.H{"state": "retrying"})
	default:
		writeAnalyticsInvalid(c, fmt.Errorf("unsupported repair kind"))
	}
}

func (h *Handler) GetAnalyticsJob(c *gin.Context) {
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	job, err := service.Maintenance().Status(c.Request.Context(), c.Param("job_id"))
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, job)
}

func (h *Handler) CancelAnalyticsJob(c *gin.Context) {
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	if err := service.Maintenance().Cancel(c.Request.Context(), c.Param("job_id")); err != nil {
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) startAnalyticsJob(c *gin.Context, kind string, options map[string]any) {
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	job, err := service.Maintenance().Start(c.Request.Context(), cpauk.MaintenanceRequest{Kind: kind, Options: options})
	if err != nil {
		if strings.Contains(err.Error(), "unsupported maintenance job") {
			writeAnalyticsInvalid(c, err)
			return
		}
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusAccepted, job)
}
