package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/concourse/atc"
	"github.com/concourse/atc/event"
	"github.com/lib/pq"
	"github.com/pivotal-golang/lager"
)

//go:generate counterfeiter . BuildDBFactory

type BuildDBFactory interface {
	GetBuildDB(build Build) BuildDB
}

const buildColumns = "id, name, job_id, team_id, status, scheduled, inputs_determined, engine, engine_metadata, start_time, end_time, reap_time"
const qualifiedBuildColumns = "b.id, b.name, b.job_id, b.team_id, b.status, b.scheduled, b.inputs_determined, b.engine, b.engine_metadata, b.start_time, b.end_time, b.reap_time, j.name as job_name, p.id as pipeline_id, p.name as pipeline_name, t.name as team_name"

func NewBuildDBFactory(conn Conn, bus *notificationsBus) BuildDBFactory {
	return &buildDBFactory{
		conn: conn,
		bus:  bus,
	}
}

type buildDBFactory struct {
	conn Conn
	bus  *notificationsBus
}

func (f *buildDBFactory) GetBuildDB(build Build) BuildDB {
	return &buildDB{
		build:      build,
		buildID:    build.ID,
		pipelineID: build.PipelineID,
		conn:       f.conn,
		bus:        f.bus,
	}
}

//go:generate counterfeiter . BuildDB

type BuildDB interface {
	Get() (Build, bool, error)
	GetID() int
	GetName() string
	GetJobName() string
	GetPipelineName() string
	GetTeamName() string
	GetEngineMetadata() string
	IsOneOff() bool

	Events(from uint) (EventSource, error)
	SaveEvent(event atc.Event) error

	GetVersionedResources() (SavedVersionedResources, error)
	GetResources() ([]BuildInput, []BuildOutput, error)

	Start(string, string) (bool, error)
	Finish(status Status) error
	MarkAsFailed(cause error) error
	Abort() error
	AbortNotifier() (Notifier, error)

	LeaseScheduling(logger lager.Logger, interval time.Duration) (Lease, bool, error)
	LeaseTracking(logger lager.Logger, interval time.Duration) (Lease, bool, error)

	GetPreparation() (BuildPreparation, bool, error)

	SaveEngineMetadata(engineMetadata string) error

	SaveInput(input BuildInput) (SavedVersionedResource, error)
	SaveOutput(vr VersionedResource, explicit bool) (SavedVersionedResource, error)

	SaveImageResourceVersion(planID atc.PlanID, identifier ResourceCacheIdentifier) error
	GetImageResourceCacheIdentifiers() ([]ResourceCacheIdentifier, error)

	GetConfig() (atc.Config, ConfigVersion, error)
}

type buildDB struct {
	buildID    int
	pipelineID int
	build      Build
	conn       Conn
	bus        *notificationsBus

	buildPrepHelper buildPreparationHelper
}

func (db *buildDB) Get() (Build, bool, error) {
	return scanBuild(db.conn.QueryRow(`
		SELECT `+qualifiedBuildColumns+`
		FROM builds b
		LEFT OUTER JOIN jobs j ON b.job_id = j.id
		LEFT OUTER JOIN pipelines p ON j.pipeline_id = p.id
		LEFT OUTER JOIN teams t ON b.team_id = t.id
		WHERE b.id = $1
	`, db.buildID))
}

func (db *buildDB) GetID() int {
	return db.buildID
}

func (db *buildDB) GetName() string {
	return db.build.Name
}

func (db *buildDB) GetJobName() string {
	return db.build.JobName
}

func (db *buildDB) GetPipelineName() string {
	return db.build.PipelineName
}

func (db *buildDB) GetTeamName() string {
	return db.build.TeamName
}

func (db *buildDB) GetEngineMetadata() string {
	return db.build.EngineMetadata
}

func (db *buildDB) IsOneOff() bool {
	return db.build.JobName == ""
}

func (db *buildDB) Events(from uint) (EventSource, error) {
	notifier, err := newConditionNotifier(db.bus, buildEventsChannel(db.buildID), func() (bool, error) {
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	table := "build_events"
	if db.pipelineID != 0 {
		table = fmt.Sprintf("pipeline_build_events_%d", db.pipelineID)
	}

	return newSQLDBBuildEventSource(
		db.buildID,
		table,
		db.conn,
		notifier,
		from,
	), nil
}

func (db *buildDB) Start(engine, metadata string) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}

	defer tx.Rollback()

	var startTime time.Time

	err = tx.QueryRow(`
		UPDATE builds
		SET status = 'started', start_time = now(), engine = $2, engine_metadata = $3
		WHERE id = $1
		AND status = 'pending'
		RETURNING start_time
	`, db.buildID, engine, metadata).Scan(&startTime)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}

		return false, err
	}

	err = db.saveEvent(tx, event.Status{
		Status: atc.StatusStarted,
		Time:   startTime.Unix(),
	})
	if err != nil {
		return false, err
	}

	err = tx.Commit()
	if err != nil {
		return false, err
	}

	err = db.bus.Notify(buildEventsChannel(db.buildID))
	if err != nil {
		return false, err
	}

	return true, nil
}

func (db *buildDB) Abort() error {
	_, err := db.conn.Exec(`
   UPDATE builds
   SET status = 'aborted'
   WHERE id = $1
 `, db.buildID)
	if err != nil {
		return err
	}

	err = db.bus.Notify(buildAbortChannel(db.buildID))
	if err != nil {
		return err
	}

	return nil
}

func (db *buildDB) AbortNotifier() (Notifier, error) {
	return newConditionNotifier(db.bus, buildAbortChannel(db.buildID), func() (bool, error) {
		var aborted bool
		err := db.conn.QueryRow(`
			SELECT status = 'aborted'
			FROM builds
			WHERE id = $1
		`, db.buildID).Scan(&aborted)

		return aborted, err
	})
}

func (db *buildDB) Finish(status Status) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}

	defer tx.Rollback()

	var endTime time.Time

	err = tx.QueryRow(`
		UPDATE builds
		SET status = $2, end_time = now(), completed = true
		WHERE id = $1
		RETURNING end_time
	`, db.buildID, string(status)).Scan(&endTime)
	if err != nil {
		return err
	}

	err = db.saveEvent(tx, event.Status{
		Status: atc.BuildStatus(status),
		Time:   endTime.Unix(),
	})
	if err != nil {
		return err
	}

	_, err = tx.Exec(fmt.Sprintf(`
		DROP SEQUENCE %s
	`, buildEventSeq(db.buildID)))
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	err = db.bus.Notify(buildEventsChannel(db.buildID))
	if err != nil {
		return err
	}

	return nil
}

func (db *buildDB) MarkAsFailed(cause error) error {
	err := db.SaveEvent(event.Error{
		Message: cause.Error(),
	})
	if err != nil {
		return err
	}

	return db.Finish(StatusErrored)
}

func (db *buildDB) SaveEvent(event atc.Event) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}

	defer tx.Rollback()

	err = db.saveEvent(tx, event)
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	err = db.bus.Notify(buildEventsChannel(db.buildID))
	if err != nil {
		return err
	}

	return nil
}

func (db *buildDB) GetResources() ([]BuildInput, []BuildOutput, error) {
	inputs := []BuildInput{}
	outputs := []BuildOutput{}

	rows, err := db.conn.Query(`
		SELECT i.name, r.name, v.type, v.version, v.metadata, r.pipeline_id,
		NOT EXISTS (
			SELECT 1
			FROM build_inputs ci, builds cb
			WHERE versioned_resource_id = v.id
			AND cb.job_id = b.job_id
			AND ci.build_id = cb.id
			AND ci.build_id < b.id
		)
		FROM versioned_resources v, build_inputs i, builds b, resources r
		WHERE b.id = $1
		AND i.build_id = b.id
		AND i.versioned_resource_id = v.id
    AND r.id = v.resource_id
		AND NOT EXISTS (
			SELECT 1
			FROM build_outputs o
			WHERE o.versioned_resource_id = v.id
			AND o.build_id = i.build_id
			AND o.explicit
		)
	`, db.buildID)
	if err != nil {
		return nil, nil, err
	}

	defer rows.Close()

	for rows.Next() {
		var inputName string
		var vr VersionedResource
		var firstOccurrence bool

		var version, metadata string
		err := rows.Scan(&inputName, &vr.Resource, &vr.Type, &version, &metadata, &vr.PipelineID, &firstOccurrence)
		if err != nil {
			return nil, nil, err
		}

		err = json.Unmarshal([]byte(version), &vr.Version)
		if err != nil {
			return nil, nil, err
		}

		err = json.Unmarshal([]byte(metadata), &vr.Metadata)
		if err != nil {
			return nil, nil, err
		}

		inputs = append(inputs, BuildInput{
			Name:              inputName,
			VersionedResource: vr,
			FirstOccurrence:   firstOccurrence,
		})
	}

	rows, err = db.conn.Query(`
		SELECT r.name, v.type, v.version, v.metadata, r.pipeline_id
		FROM versioned_resources v, build_outputs o, builds b, resources r
		WHERE b.id = $1
		AND o.build_id = b.id
		AND o.versioned_resource_id = v.id
    AND r.id = v.resource_id
		AND o.explicit
	`, db.buildID)
	if err != nil {
		return nil, nil, err
	}

	defer rows.Close()

	for rows.Next() {
		var vr VersionedResource

		var version, metadata string
		err := rows.Scan(&vr.Resource, &vr.Type, &version, &metadata, &vr.PipelineID)
		if err != nil {
			return nil, nil, err
		}

		err = json.Unmarshal([]byte(version), &vr.Version)
		if err != nil {
			return nil, nil, err
		}

		err = json.Unmarshal([]byte(metadata), &vr.Metadata)
		if err != nil {
			return nil, nil, err
		}

		outputs = append(outputs, BuildOutput{
			VersionedResource: vr,
		})
	}

	return inputs, outputs, nil
}

func (db *buildDB) getVersionedResources(resourceRequest string) (SavedVersionedResources, error) {
	rows, err := db.conn.Query(resourceRequest, db.buildID)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	savedVersionedResources := SavedVersionedResources{}

	for rows.Next() {
		var versionedResource SavedVersionedResource
		var versionJSON []byte
		var metadataJSON []byte
		err = rows.Scan(&versionedResource.ID, &versionedResource.Enabled, &versionJSON, &metadataJSON, &versionedResource.Type, &versionedResource.Resource, &versionedResource.PipelineID, &versionedResource.ModifiedTime)

		err = json.Unmarshal(versionJSON, &versionedResource.Version)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(metadataJSON, &versionedResource.Metadata)
		if err != nil {
			return nil, err
		}

		savedVersionedResources = append(savedVersionedResources, versionedResource)
	}

	return savedVersionedResources, nil
}

func (db *buildDB) GetVersionedResources() (SavedVersionedResources, error) {
	return db.getVersionedResources(`
		SELECT vr.id,
			vr.enabled,
			vr.version,
			vr.metadata,
			vr.type,
			r.name,
			r.pipeline_id,
			vr.modified_time
		FROM builds b
		INNER JOIN jobs j ON b.job_id = j.id
		INNER JOIN build_inputs bi ON bi.build_id = b.id
		INNER JOIN versioned_resources vr ON bi.versioned_resource_id = vr.id
		INNER JOIN resources r ON vr.resource_id = r.id
		WHERE b.id = $1

		UNION ALL

		SELECT vr.id,
			vr.enabled,
			vr.version,
			vr.metadata,
			vr.type,
			r.name,
			r.pipeline_id,
			vr.modified_time
		FROM builds b
		INNER JOIN jobs j ON b.job_id = j.id
		INNER JOIN build_outputs bo ON bo.build_id = b.id
		INNER JOIN versioned_resources vr ON bo.versioned_resource_id = vr.id
		INNER JOIN resources r ON vr.resource_id = r.id
		WHERE b.id = $1 AND bo.explicit`)
}

func (db *buildDB) LeaseScheduling(logger lager.Logger, interval time.Duration) (Lease, bool, error) {
	lease := &lease{
		conn: db.conn,
		logger: logger.Session("lease", lager.Data{
			"build_id": db.buildID,
		}),
		attemptSignFunc: func(tx Tx) (sql.Result, error) {
			return tx.Exec(`
				UPDATE builds
				SET last_scheduled = now()
				WHERE id = $1
					AND now() - last_scheduled > ($2 || ' SECONDS')::INTERVAL
			`, db.buildID, interval.Seconds())
		},
		heartbeatFunc: func(tx Tx) (sql.Result, error) {
			return tx.Exec(`
				UPDATE builds
				SET last_scheduled = now()
				WHERE id = $1
			`, db.buildID)
		},
	}

	renewed, err := lease.AttemptSign(interval)
	if err != nil {
		return nil, false, err
	}

	if !renewed {
		return nil, renewed, nil
	}

	lease.KeepSigned(interval)

	return lease, true, nil
}

func (db *buildDB) GetPreparation() (BuildPreparation, bool, error) {
	return db.buildPrepHelper.GetBuildPreparation(db.conn, db.buildID)
}

func (db *buildDB) SaveInput(input BuildInput) (SavedVersionedResource, error) {
	row := db.conn.QueryRow(`
		SELECT `+pipelineColumns+`
		FROM pipelines
		WHERE id = $1
	`, input.VersionedResource.PipelineID)

	savedPipeline, err := scanPipeline(row)
	if err != nil {
		return SavedVersionedResource{}, err
	}
	pipelineDBFactory := NewPipelineDBFactory(db.conn, db.bus)
	pipelineDB := pipelineDBFactory.Build(savedPipeline)

	return pipelineDB.SaveInput(db.buildID, input)
}

func (db *buildDB) SaveOutput(vr VersionedResource, explicit bool) (SavedVersionedResource, error) {
	row := db.conn.QueryRow(`
		SELECT `+pipelineColumns+`
		FROM pipelines
		WHERE id = $1
	`, vr.PipelineID)

	savedPipeline, err := scanPipeline(row)
	if err != nil {
		return SavedVersionedResource{}, err
	}
	pipelineDBFactory := NewPipelineDBFactory(db.conn, db.bus)
	pipelineDB := pipelineDBFactory.Build(savedPipeline)

	return pipelineDB.SaveOutput(db.buildID, vr, explicit)
}

func (db *buildDB) SaveEngineMetadata(engineMetadata string) error {
	_, err := db.conn.Exec(`
		UPDATE builds
		SET engine_metadata = $2
		WHERE id = $1
	`, db.buildID, engineMetadata)
	if err != nil {
		return err
	}

	return nil
}

func (db *buildDB) SaveImageResourceVersion(planID atc.PlanID, identifier ResourceCacheIdentifier) error {
	version, err := json.Marshal(identifier.ResourceVersion)
	if err != nil {
		return err
	}

	result, err := db.conn.Exec(`
		UPDATE image_resource_versions
		SET version = $1, resource_hash = $4
		WHERE build_id = $2 AND plan_id = $3
	`, version, db.buildID, string(planID), identifier.ResourceHash)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		_, err := db.conn.Exec(`
			INSERT INTO image_resource_versions(version, build_id, plan_id, resource_hash)
			VALUES ($1, $2, $3, $4)
		`, version, db.buildID, string(planID), identifier.ResourceHash)
		if err != nil {
			return err
		}
	}

	return nil
}

func (db *buildDB) GetImageResourceCacheIdentifiers() ([]ResourceCacheIdentifier, error) {
	rows, err := db.conn.Query(`
  	SELECT version, resource_hash
  	FROM image_resource_versions
  	WHERE build_id = $1
  `, db.buildID)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var identifiers []ResourceCacheIdentifier

	for rows.Next() {
		var identifier ResourceCacheIdentifier
		var marshalledVersion []byte

		err := rows.Scan(&marshalledVersion, &identifier.ResourceHash)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(marshalledVersion, &identifier.ResourceVersion)
		if err != nil {
			return nil, err
		}

		identifiers = append(identifiers, identifier)
	}

	return identifiers, nil
}

func (db *buildDB) LeaseTracking(logger lager.Logger, interval time.Duration) (Lease, bool, error) {
	lease := &lease{
		conn: db.conn,
		logger: logger.Session("lease", lager.Data{
			"build_id": db.buildID,
		}),
		attemptSignFunc: func(tx Tx) (sql.Result, error) {
			return tx.Exec(`
				UPDATE builds
				SET last_tracked = now()
				WHERE id = $1
					AND now() - last_tracked > ($2 || ' SECONDS')::INTERVAL
			`, db.buildID, interval.Seconds())
		},
		heartbeatFunc: func(tx Tx) (sql.Result, error) {
			return tx.Exec(`
				UPDATE builds
				SET last_tracked = now()
				WHERE id = $1
			`, db.buildID)
		},
	}

	renewed, err := lease.AttemptSign(interval)
	if err != nil {
		return nil, false, err
	}

	if !renewed {
		return nil, renewed, nil
	}

	lease.KeepSigned(interval)

	return lease, true, nil
}

func (db *buildDB) GetConfig() (atc.Config, ConfigVersion, error) {
	var configBlob []byte
	var version int
	err := db.conn.QueryRow(`
			SELECT p.config, p.version
			FROM builds b
			INNER JOIN jobs j ON b.job_id = j.id
			INNER JOIN pipelines p ON j.pipeline_id = p.id
			WHERE b.ID = $1
		`, db.buildID).Scan(&configBlob, &version)
	if err != nil {
		if err == sql.ErrNoRows {
			return atc.Config{}, 0, nil
		} else {
			return atc.Config{}, 0, err
		}
	}

	var config atc.Config
	err = json.Unmarshal(configBlob, &config)
	if err != nil {
		return atc.Config{}, 0, err
	}

	return config, ConfigVersion(version), nil
}

func newConditionNotifier(bus *notificationsBus, channel string, cond func() (bool, error)) (Notifier, error) {
	notified, err := bus.Listen(channel)
	if err != nil {
		return nil, err
	}

	notifier := &conditionNotifier{
		cond:    cond,
		bus:     bus,
		channel: channel,

		notified: notified,
		notify:   make(chan struct{}, 1),

		stop: make(chan struct{}),
	}

	go notifier.watch()

	return notifier, nil
}

func (db *buildDB) saveEvent(tx Tx, event atc.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	table := "build_events"
	if db.pipelineID != 0 {
		table = fmt.Sprintf("pipeline_build_events_%d", db.pipelineID)
	}

	_, err = tx.Exec(fmt.Sprintf(`
		INSERT INTO %s (event_id, build_id, type, version, payload)
		VALUES (nextval('%s'), $1, $2, $3, $4)
	`, table, buildEventSeq(db.buildID)), db.buildID, string(event.EventType()), string(event.Version()), payload)
	if err != nil {
		return err
	}

	return nil
}

func scanBuild(row scannable) (Build, bool, error) {
	var id int
	var name string
	var jobID, pipelineID sql.NullInt64
	var status string
	var scheduled bool
	var inputsDetermined bool
	var engine, engineMetadata, jobName, pipelineName sql.NullString
	var startTime pq.NullTime
	var endTime pq.NullTime
	var reapTime pq.NullTime
	var teamID int
	var teamName string

	err := row.Scan(&id, &name, &jobID, &teamID, &status, &scheduled, &inputsDetermined, &engine, &engineMetadata, &startTime, &endTime, &reapTime, &jobName, &pipelineID, &pipelineName, &teamName)
	if err != nil {
		if err == sql.ErrNoRows {
			return Build{}, false, nil
		}

		return Build{}, false, err
	}

	build := Build{
		ID:               id,
		Name:             name,
		Status:           Status(status),
		Scheduled:        scheduled,
		InputsDetermined: inputsDetermined,

		Engine:         engine.String,
		EngineMetadata: engineMetadata.String,

		StartTime: startTime.Time,
		EndTime:   endTime.Time,
		ReapTime:  reapTime.Time,

		TeamID:   teamID,
		TeamName: teamName,
	}

	if jobID.Valid {
		build.JobID = int(jobID.Int64)
		build.JobName = jobName.String
		build.PipelineName = pipelineName.String
		build.PipelineID = int(pipelineID.Int64)
	}

	return build, true, nil
}

func buildAbortChannel(buildID int) string {
	return fmt.Sprintf("build_abort_%d", buildID)
}

func buildEventsChannel(buildID int) string {
	return fmt.Sprintf("build_events_%d", buildID)
}

func buildEventSeq(buildID int) string {
	return fmt.Sprintf("build_event_id_seq_%d", buildID)
}
