package resourceserver

import (
	"net/http"
	"strconv"

	"github.com/tedsuo/rata"
)

func (s *Server) DisableResourceVersion(w http.ResponseWriter, r *http.Request) {
	resourceID, err := strconv.Atoi(rata.Param(r, "resource_version_id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	err = s.resourceDB.DisableVersionedResource(resourceID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
