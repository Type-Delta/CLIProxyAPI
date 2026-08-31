package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type managementCapabilities struct {
	ManagementAPIVersion int                `json:"management_api_version"`
	Analytics            model.Capabilities `json:"analytics"`
	APIKeys              apiKeyCapabilities `json:"api_keys"`
}

type apiKeyCapabilities struct {
	StructuredEntries bool `json:"structured_entries"`
	RevisionedWrites  bool `json:"revisioned_writes"`
	KeyIDV1           bool `json:"key_id_v1"`
}

func (h *Handler) GetCapabilities(c *gin.Context) {
	capabilities := model.Capabilities{
		APISchemaVersions: []int{model.APISchemaVersion}, EventSchemaVersion: model.EventSchemaVersion,
		Supported: true, State: model.StateDisabled, StorageDriver: "sqlite", StorageScope: "instance",
		KeyIDAlgorithm: model.KeyIDAlgorithm, StructuredKeys: true, SharedEnforcement: false,
		ManagementQueryV1: true, ViewerV1: true,
	}
	if service := h.analyticsService(); service != nil {
		capabilities = service.Capabilities()
	}
	h.mu.Lock()
	capabilities.ViewerV1 = h.analyticsViewerAvailable
	h.mu.Unlock()
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, managementCapabilities{
		ManagementAPIVersion: 1,
		Analytics:            capabilities,
		APIKeys: apiKeyCapabilities{
			StructuredEntries: true, RevisionedWrites: true, KeyIDV1: true,
		},
	})
}

func (h *Handler) GetAnalyticsHealth(c *gin.Context) {
	service := h.analyticsService()
	if service == nil {
		service = cpauk.NewUnavailable("not_initialized", cpauk.DefaultConfig())
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, service.Health())
}
