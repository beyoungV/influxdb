package write

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/influxdata/influxdb/models"
	influxdb "github.com/influxdata/influxdb/servicesv2"
	kithttp "github.com/influxdata/influxdb/servicesv2/kit/http"
	"go.uber.org/zap"
)

// WriteHandler represents an HTTP API handler for write requests.
type WriteHandler struct {
	chi.Router
	api      *kithttp.API
	writeSvc *Service

	bucketSvc influxdb.BucketService
	orgSvc    influxdb.OrganizationService
	dbrpSvc   influxdb.DBRPMappingServiceV2
}

const (
	v1Prefix = "/write"
	prefix   = "/api/v2/write"
)

// NewHTTPWriteHandler constructs a new http server.
func NewHTTPWriteHandler(writeSvc *Service, orgSvc influxdb.OrganizationService, bucketSvc influxdb.BucketService, dbrpSvc influxdb.DBRPMappingServiceV2) *WriteHandler {
	logger, _ := zap.NewDevelopment()
	svr := &WriteHandler{
		api:       kithttp.NewAPI(kithttp.WithLog(logger)),
		writeSvc:  writeSvc,
		bucketSvc: bucketSvc,
		orgSvc:    orgSvc,
		dbrpSvc:   dbrpSvc,
	}

	r := chi.NewRouter()
	r.Use(
		middleware.Recoverer,
		middleware.RequestID,
		middleware.RealIP,
		middleware.Logger,
	)

	r.Post("/", svr.handleWrite)

	svr.Router = r
	return svr
}

type resourceHandler struct {
	prefix string
	*WriteHandler
}

func (h *resourceHandler) Prefix() string {
	return h.prefix
}
func (h *WriteHandler) V1ResourceHandler() *resourceHandler {
	return &resourceHandler{prefix: v1Prefix, WriteHandler: h}
}

func (h *WriteHandler) V2ResourceHandler() *resourceHandler {
	return &resourceHandler{prefix: prefix, WriteHandler: h}
}

func (h *WriteHandler) handleWrite(w http.ResponseWriter, r *http.Request) { // bookmark (al)
	// lookup bucket
	precision := r.URL.Query().Get("precision")
	switch precision {
	case "ns":
		precision = "n"
	case "us":
		precision = "u"
	case "ms", "s", "":
		// same as v1 so do nothing
	default:
		err := fmt.Errorf("invalid precision %q (use ns, us, ms or s)", precision)
		h.api.Err(w, r, err)
		return
	}

	switch precision {
	case "", "n", "ns", "u", "ms", "s", "m", "h":
		// it's valid
	default:
		err := fmt.Errorf("invalid precision %q (use n, u, ms, s, m or h)", precision)
		h.api.Err(w, r, err)
		return
	}

	org, bucket, err := h.findTenantV2(r.Context(), r)
	if err != nil {
		var v1err error
		org, bucket, v1err = h.findTenantV1(r.Context(), r)
		if v1err != nil {
			h.api.Err(w, r, err)
			return
		}
	}

	// parse points
	points, err := h.parsePoints(r.Context(), r, precision)
	if err != nil {
		if err.Error() == "EOF" && len(points) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.api.Err(w, r, err)
		return
	}

	// write
	if err := h.writeSvc.WritePoints(r.Context(), org.ID, bucket.ID, points); err != nil {
		h.api.Err(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *WriteHandler) parsePoints(ctx context.Context, r *http.Request, precision string) ([]models.Point, error) {
	body := r.Body
	// Handle gzip decoding of the body
	if r.Header.Get("Content-Encoding") == "gzip" {
		b, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer b.Close()
		body = b
	}

	var bs []byte
	if r.ContentLength > 0 {
		// This will just be an initial hint for the gzip reader, as the
		// bytes.Buffer will grow as needed when ReadFrom is called
		bs = make([]byte, 0, r.ContentLength)
	}

	buf := bytes.NewBuffer(bs)

	_, err := buf.ReadFrom(body)
	if err != nil {
		return nil, err
	}
	return models.ParsePointsWithPrecision(buf.Bytes(), time.Now().UTC(), precision)
}

func (h *WriteHandler) findTenantV2(ctx context.Context, r *http.Request) (*influxdb.Organization, *influxdb.Bucket, error) {
	org, err := h.findOrganization(r.Context(), r)
	if err != nil {
		return nil, nil, err
	}

	bucket, err := h.findBucket(r.Context(), r, org.ID)
	if err != nil {
		return nil, nil, err
	}

	return org, bucket, nil
}

func (h *WriteHandler) findTenantV1(ctx context.Context, r *http.Request) (*influxdb.Organization, *influxdb.Bucket, error) {
	db := r.URL.Query().Get("db")
	rp := r.URL.Query().Get("rp")

	dbrps, _, err := h.dbrpSvc.FindMany(ctx, influxdb.DBRPMappingFilterV2{
		Database:        &db,
		RetentionPolicy: &rp,
	})
	if err != nil {
		return nil, nil, err
	}

	if len(dbrps) != 1 {
		return nil, nil, fmt.Errorf("failed for find DBRP mapping for db:%q, rp:%q", db, rp)
	}

	org, err := h.orgSvc.FindOrganizationByID(ctx, dbrps[0].OrganizationID)
	if err != nil {
		return nil, nil, err
	}

	bucket, err := h.bucketSvc.FindBucketByID(ctx, dbrps[0].BucketID)
	if err != nil {
		return nil, nil, err
	}

	return org, bucket, nil
}

func (h *WriteHandler) findOrganization(ctx context.Context, r *http.Request) (*influxdb.Organization, error) {
	filter := influxdb.OrganizationFilter{}
	if organization := r.URL.Query().Get("org"); organization != "" {
		if id, err := influxdb.IDFromString(organization); err == nil {
			filter.ID = id
		} else {
			filter.Name = &organization
		}
	}

	if reqID := r.URL.Query().Get("org_id"); reqID != "" {
		var err error
		filter.ID, err = influxdb.IDFromString(reqID)
		if err != nil {
			return nil, err
		}
	}
	return h.orgSvc.FindOrganization(ctx, filter)
}

func (h *WriteHandler) findBucket(ctx context.Context, r *http.Request, orgID influxdb.ID) (*influxdb.Bucket, error) {
	bucket := r.URL.Query().Get("bucket")
	if id, err := influxdb.IDFromString(bucket); err == nil {
		b, err := h.bucketSvc.FindBucket(ctx, influxdb.BucketFilter{
			OrganizationID: &orgID,
			ID:             id,
		})
		if err != nil && influxdb.ErrorCode(err) != influxdb.ENotFound {
			return nil, err
		} else if err == nil {
			return b, err
		}
	}

	return h.bucketSvc.FindBucket(ctx, influxdb.BucketFilter{
		OrganizationID: &orgID,
		Name:           &bucket,
	})
}