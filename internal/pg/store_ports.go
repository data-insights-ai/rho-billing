package pg

import (
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

func (s *Store) Credits() credit.Repository { return s }

func (s *Store) Queue() integration.Repository { return s }
