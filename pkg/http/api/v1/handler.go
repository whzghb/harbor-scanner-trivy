package v1

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/aquasecurity/harbor-scanner-trivy/pkg/etc"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/harbor"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/http/api"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/job"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/persistence"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/queue"
	"github.com/aquasecurity/harbor-scanner-trivy/pkg/trivy"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	pathVarScanRequestID = "scan_request_id"

	propertyScannerType        = "harbor.scanner-adapter/scanner-type"
	propertyDBUpdatedAt        = "harbor.scanner-adapter/vulnerability-database-updated-at"
	propertyDBNextUpdateAt     = "harbor.scanner-adapter/vulnerability-database-next-update-at"
	propertyJavaDBUpdatedAt    = "harbor.scanner-adapter/vulnerability-java-database-updated-at"
	propertyJavaDBNextUpdateAt = "harbor.scanner-adapter/vulnerability-java-database-next-update-at"
)

type requestHandler struct {
	info     etc.BuildInfo
	config   etc.Config
	enqueuer queue.Enqueuer
	store    persistence.Store
	wrapper  trivy.Wrapper
	api.BaseHandler
}

func NewAPIHandler(info etc.BuildInfo, config etc.Config, enqueuer queue.Enqueuer, store persistence.Store, wrapper trivy.Wrapper) http.Handler {
	handler := &requestHandler{
		info:     info,
		config:   config,
		enqueuer: enqueuer,
		store:    store,
		wrapper:  wrapper,
	}

	router := mux.NewRouter()
	router.Use(handler.logRequest)

	apiV1Router := router.PathPrefix("/api/v1").Subrouter()
	apiV1Router.Methods(http.MethodPost).Path("/scan").HandlerFunc(handler.AcceptScanRequest)
	apiV1Router.Methods(http.MethodGet).Path("/scan/{scan_request_id}/report").HandlerFunc(handler.GetScanReport)
	apiV1Router.Methods(http.MethodGet).Path("/metadata").HandlerFunc(handler.GetMetadata)

	probeRouter := router.PathPrefix("/probe").Subrouter()
	probeRouter.Methods(http.MethodGet).Path("/healthy").HandlerFunc(handler.GetHealthy)
	probeRouter.Methods(http.MethodGet).Path("/ready").HandlerFunc(handler.GetReady)

	if config.API.MetricsEnabled {
		router.Methods(http.MethodGet).Path("/metrics").Handler(promhttp.Handler())
	}

	return router
}

func (h *requestHandler) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("Request",
			slog.String("addr", r.RemoteAddr),
			slog.String("proto", r.Proto),
			slog.String("method", r.Method),
			slog.String("uri", r.URL.RequestURI()),
		)
		next.ServeHTTP(w, r)
	})
}

func (h *requestHandler) AcceptScanRequest(res http.ResponseWriter, req *http.Request) {
	scanRequest := harbor.ScanRequest{}
	if err := json.NewDecoder(req.Body).Decode(&scanRequest); err != nil {
		slog.Error("Error while unmarshalling scan request", slog.String("err", err.Error()))
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusBadRequest,
			Message:  fmt.Sprintf("unmarshalling scan request: %s", err.Error()),
		})
		return
	}

	if validationError := h.ValidateScanRequest(scanRequest); validationError != nil {
		slog.Error("Error while validating scan request", slog.String("err", validationError.Message))
		h.WriteJSONError(res, *validationError)
		return
	}

	scanJob, err := h.enqueuer.Enqueue(req.Context(), scanRequest)
	if err != nil {
		slog.Error("Error while enqueuing scan job", slog.String("err", err.Error()))
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusInternalServerError,
			Message:  fmt.Sprintf("enqueuing scan job: %s", err.Error()),
		})
		return
	}

	scanResponse := harbor.ScanResponse{ID: scanJob.ID}

	h.WriteJSON(res, scanResponse, api.MimeTypeScanResponse, http.StatusAccepted)
}

func (h *requestHandler) ValidateScanRequest(req harbor.ScanRequest) *harbor.Error {
	if req.Registry.URL == "" {
		return &harbor.Error{
			HTTPCode: http.StatusUnprocessableEntity,
			Message:  "missing registry.url",
		}
	}

	if _, err := url.ParseRequestURI(req.Registry.URL); err != nil {
		return &harbor.Error{
			HTTPCode: http.StatusUnprocessableEntity,
			Message:  "invalid registry.url",
		}
	}

	if req.Artifact.Repository == "" {
		return &harbor.Error{
			HTTPCode: http.StatusUnprocessableEntity,
			Message:  "missing artifact.repository",
		}
	}

	if req.Artifact.Digest == "" {
		return &harbor.Error{
			HTTPCode: http.StatusUnprocessableEntity,
			Message:  "missing artifact.digest",
		}
	}

	return nil
}

func (h *requestHandler) GetScanReport(res http.ResponseWriter, req *http.Request) {
	var reportMimeType api.MimeType

	if err := reportMimeType.FromAcceptHeader(req.Header.Get(api.HeaderAccept)); err != nil {
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusUnsupportedMediaType,
			Message:  fmt.Sprintf("unsupported media type %s", req.Header.Get(api.HeaderAccept)),
		})
		return
	}

	vars := mux.Vars(req)
	scanJobID, ok := vars[pathVarScanRequestID]
	if !ok {
		slog.Error("Error while parsing `scan_request_id` path variable")
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusBadRequest,
			Message:  "missing scan_request_id",
		})
		return
	}

	reqLog := slog.With(slog.String("scan_job_id", scanJobID))

	scanJob, err := h.store.Get(req.Context(), scanJobID)
	if err != nil {
		reqLog.Error("Error while getting scan job")
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusInternalServerError,
			Message:  fmt.Sprintf("getting scan job: %v", err),
		})
		return
	}

	if scanJob == nil {
		reqLog.Error("Cannot find scan job")
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusNotFound,
			Message:  fmt.Sprintf("cannot find scan job: %v", scanJobID),
		})
		return
	}

	scanJobLog := reqLog.With(slog.String("scan_job_status", scanJob.Status.String()))

	if scanJob.Status == job.Queued || scanJob.Status == job.Pending {
		scanJobLog.Debug("Scan job has not finished yet")
		res.Header().Add("Location", req.URL.String())
		res.WriteHeader(http.StatusFound)
		return
	}

	if scanJob.Status == job.Failed {
		scanJobLog.Error("Scan job failed", slog.String("err", scanJob.Error))
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusInternalServerError,
			Message:  scanJob.Error,
		})
		return
	}

	if scanJob.Status != job.Finished {
		scanJobLog.Error("Unexpected scan job status")
		h.WriteJSONError(res, harbor.Error{
			HTTPCode: http.StatusInternalServerError,
			Message:  fmt.Sprintf("unexpected status %v of scan job %v", scanJob.Status, scanJob.ID),
		})
		return
	}

	h.WriteJSON(res, scanJob.Report, reportMimeType, http.StatusOK)
}

func (h *requestHandler) GetMetadata(res http.ResponseWriter, _ *http.Request) {
	properties := map[string]string{
		propertyScannerType: "os-package-vulnerability",

		"org.label-schema.version":    h.info.Version,
		"org.label-schema.build-date": h.info.Date,
		"org.label-schema.vcs-ref":    h.info.Commit,
		"org.label-schema.vcs":        "https://github.com/aquasecurity/harbor-scanner-trivy",

		"env.SCANNER_TRIVY_SKIP_UPDATE":         strconv.FormatBool(h.config.Trivy.SkipUpdate),
		"env.SCANNER_TRIVY_SKIP_JAVA_DB_UPDATE": strconv.FormatBool(h.config.Trivy.SkipJavaDBUpdate),
		"env.SCANNER_TRIVY_OFFLINE_SCAN":        strconv.FormatBool(h.config.Trivy.OfflineScan),
		"env.SCANNER_TRIVY_IGNORE_UNFIXED":      strconv.FormatBool(h.config.Trivy.IgnoreUnfixed),
		"env.SCANNER_TRIVY_DEBUG_MODE":          strconv.FormatBool(h.config.Trivy.DebugMode),
		"env.SCANNER_TRIVY_INSECURE":            strconv.FormatBool(h.config.Trivy.Insecure),
		"env.SCANNER_TRIVY_VULN_TYPE":           h.config.Trivy.VulnType,
		"env.SCANNER_TRIVY_SECURITY_CHECKS":     h.config.Trivy.SecurityChecks,
		"env.SCANNER_TRIVY_SEVERITY":            h.config.Trivy.Severity,
		"env.SCANNER_TRIVY_TIMEOUT":             h.config.Trivy.Timeout.String(),
	}

	vi, err := h.wrapper.GetVersion()
	if err != nil {
		slog.Error("Error while retrieving vulnerability DB version", slog.String("err", err.Error()))
	}

	if err == nil && vi.VulnerabilityDB != nil {
		properties[propertyDBUpdatedAt] = vi.VulnerabilityDB.UpdatedAt.Format(time.RFC3339)
	}

	if err == nil && vi.VulnerabilityDB != nil && !h.config.Trivy.SkipUpdate {
		properties[propertyDBNextUpdateAt] = vi.VulnerabilityDB.NextUpdate.Format(time.RFC3339)
	}

	if err == nil && vi.JavaDB != nil && !h.config.Trivy.SkipJavaDBUpdate {
		properties[propertyJavaDBNextUpdateAt] = vi.JavaDB.NextUpdate.Format(time.RFC3339)
	}

	metadata := &harbor.ScannerAdapterMetadata{
		Scanner: etc.GetScannerMetadata(),
		Capabilities: []harbor.Capability{
			{
				ConsumesMIMETypes: []string{
					api.MimeTypeOCIImageManifest.String(),
					api.MimeTypeDockerImageManifestV2.String(),
				},
				ProducesMIMETypes: []string{
					api.MimeTypeSecurityVulnerabilityReport.String(),
				},
			},
		},
		Properties: properties,
	}
	h.WriteJSON(res, metadata, api.MimeTypeMetadata, http.StatusOK)
}

func (h *requestHandler) GetHealthy(res http.ResponseWriter, req *http.Request) {
	res.WriteHeader(http.StatusOK)
}

func (h *requestHandler) GetReady(res http.ResponseWriter, req *http.Request) {
	res.WriteHeader(http.StatusOK)
}
