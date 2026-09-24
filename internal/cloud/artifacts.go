package cloud

import (
	"context"
	"encoding/json"
	"fmt"

	api "thermora.local/thermora/internal/transport/http/generated"
)

func (a *App) comparisonArtifact(ctx context.Context, device, id string) ([]byte, string, error) {
	if _, err := a.device(ctx, device, request(ctx).userID); err != nil {
		return nil, "", err
	}
	var data []byte
	var org string
	err := a.DB.QueryRow(
		ctx,
		`SELECT report,organization_id FROM comparisons WHERE id=$1 AND device_id=$2`,
		id,
		device,
	).Scan(
		&data,
		&org,
	)
	if err != nil {
		return nil, "", err
	}
	if a.Artifacts == nil {
		return nil, "", fail(503, "artifacts_unavailable", "Report storage is available in the local cloud profile.")
	}
	return data, fmt.Sprintf("%s/%s/%s.json", org, device, id), nil
}

func (a *App) ExportComparison(
	ctx context.Context,
	r api.ExportComparisonRequestObject,
) (api.ExportComparisonResponseObject, error) {
	data, key, err := a.comparisonArtifact(ctx, r.DeviceId, r.ComparisonId)
	if err != nil {
		return nil, err
	}
	if err = a.Artifacts.Put(ctx, key, data); err != nil {
		return nil, fail(
			503,
			"artifact_export_failed",
			"Report storage is unavailable. Retry this export; the comparison is preserved.",
		)
	}
	return api.ExportComparison201JSONResponse{
		DownloadUrl: fmt.Sprintf("/api/v1/devices/%s/comparisons/%s/artifact", r.DeviceId, r.ComparisonId),
	}, nil
}

func (a *App) DownloadComparison(
	ctx context.Context,
	r api.DownloadComparisonRequestObject,
) (api.DownloadComparisonResponseObject, error) {
	_, key, err := a.comparisonArtifact(ctx, r.DeviceId, r.ComparisonId)
	if err != nil {
		return nil, err
	}
	data, err := a.Artifacts.Get(ctx, key)
	if err != nil {
		return nil, fail(
			503,
			"artifact_unavailable",
			"Export this report first, or retry when report storage is available.",
		)
	}
	var report api.Comparison
	if err = json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	if report.Id != r.ComparisonId || report.DeviceId != r.DeviceId {
		return nil, fail(503, "artifact_invalid", "Stored report identity did not match.")
	}
	request(ctx).writer.Header().Set("Content-Disposition", `attachment; filename="thermora-comparison.json"`)
	return api.DownloadComparison200JSONResponse(report), nil
}
