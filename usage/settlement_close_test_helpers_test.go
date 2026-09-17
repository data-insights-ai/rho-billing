package usage

import "context"

func (s *SettlementService) finishClose(ctx context.Context, in CloseInput) (Batch, error) {
	job, err := s.StartClose(ctx, in)
	if err != nil {
		return Batch{}, err
	}
	for job.State == ClosePreparing {
		job, err = s.AdvanceClose(ctx, in.Account, job.BatchID, job.Revision, 1000)
		if err != nil {
			return Batch{}, err
		}
	}
	if job.State == CloseReady {
		job, err = s.PublishClose(ctx, in.Account, job.BatchID, job.Revision)
		if err != nil {
			return Batch{}, err
		}
	}
	summary, err := s.BatchSummary(ctx, in.Account, in.BatchID)
	if err != nil {
		return Batch{}, err
	}
	batch := Batch{Account: summary.Account, ID: summary.ID, Period: summary.Period, Currency: summary.Currency, Total: summary.Total, State: summary.State, Revision: summary.Revision, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt}
	after := ""
	for {
		lines, next, more, e := s.BatchLinesPage(ctx, in.Account, in.BatchID, after, 1000)
		if e != nil {
			return Batch{}, e
		}
		batch.Lines = append(batch.Lines, lines...)
		if !more {
			break
		}
		after = next
	}
	batch.Fingerprint = batchFingerprint(batch)
	return batch, nil
}
