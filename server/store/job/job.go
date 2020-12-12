package job

import (
	"github.com/bleenco/abstruse/server/core"
	"github.com/jinzhu/gorm"
)

// New returns new JobStore.
func New(db *gorm.DB, repos core.RepositoryStore) core.JobStore {
	return jobStore{db, repos}
}

type jobStore struct {
	db    *gorm.DB
	repos core.RepositoryStore
}

func (s jobStore) Find(id uint) (*core.Job, error) {
	var job core.Job
	err := s.db.Model(&job).Where("id = ?", id).Preload("Build.Repository.Provider").First(&job).Error
	return &job, err
}

func (s jobStore) FindUser(id, userID uint) (*core.Job, error) {
	var job core.Job
	err := s.db.Model(&job).Where("id = ?", id).Preload("Build.Repository.Provider").First(&job).Error
	if err != nil {
		return &job, err
	}
	job.Build.Repository.Perms = s.repos.GetPermissions(job.Build.RepositoryID, userID)
	return &job, err
}

func (s jobStore) Create(job *core.Job) error {
	return s.db.Create(job).Error
}

func (s jobStore) Update(job *core.Job) error {
	log := []byte(job.Log)
	if len(log) > 65535 {
		log = log[len(log)-65535:]
		job.Log = string(log)
	}

	return s.db.Model(job).Updates(map[string]interface{}{
		"status":     job.Status,
		"start_time": job.StartTime,
		"end_time":   job.EndTime,
		"log":        job.Log,
	}).Error
}

func (s jobStore) Delete(job *core.Job) error {
	return s.db.Delete(job).Error
}
