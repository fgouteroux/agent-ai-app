import React from 'react';
import { GrafanaTheme2 } from '@grafana/data';
import { useStyles2, Icon } from '@grafana/ui';
import { css } from '@emotion/css';
import { brand } from '../../../../brand';
import type { WorkerEventInfo } from '../../../../context';

interface WorkerActivityTrackerProps {
  workers: WorkerEventInfo[];
}

// Renders one chip per currently-tracked dispatched worker subagent (see
// dispatch_worker in the backend) -- several can be active at once, each
// with its own live status text. ChatInterface keeps a 'done'/'error'
// worker's chip around briefly (WORKER_CHIP_LINGER_MS) before dropping it,
// so this component only ever renders whatever's currently in `workers`,
// same as ActivityAccordion's own "just show what you're given" shape.
export const WorkerActivityTracker: React.FC<WorkerActivityTrackerProps> = ({ workers }) => {
  const styles = useStyles2(getStyles);

  if (workers.length === 0) {
    return null;
  }

  return (
    <div className={styles.container} data-testid="worker-activity-tracker">
      {workers.map((worker) => (
        <div key={worker.taskId} className={styles.chip} data-testid="worker-activity-chip">
          {worker.phase === 'running' ? (
            <Icon name="fa fa-spinner" className={styles.spinner} />
          ) : worker.phase === 'error' ? (
            <span className={styles.errorIcon}>⚠</span>
          ) : (
            <span className={styles.doneIcon}>✓</span>
          )}
          <div className={styles.textStack}>
            <span className={styles.label}>{worker.label}</span>
            <span className={styles.status}>{worker.status}</span>
          </div>
        </div>
      ))}
    </div>
  );
};

const getStyles = (theme: GrafanaTheme2) => ({
  container: css`
    display: flex;
    flex-wrap: wrap;
    gap: ${theme.spacing(1)};
    margin-bottom: ${theme.spacing(1)};
  `,
  chip: css`
    display: flex;
    align-items: center;
    gap: ${theme.spacing(1)};
    padding: ${theme.spacing(0.5)} ${theme.spacing(1)};
    border: 1px solid ${theme.colors.border.weak};
    border-radius: 6px;
    background: ${theme.colors.background.secondary};
    max-width: 320px;
  `,
  spinner: css`
    color: ${brand.red};
    font-size: 14px;
    flex-shrink: 0;
    animation: worker-chip-spin 1s linear infinite;
    @keyframes worker-chip-spin {
      0% { transform: rotate(0deg); }
      100% { transform: rotate(360deg); }
    }
  `,
  doneIcon: css`
    color: ${theme.colors.success.text};
    font-weight: bold;
    flex-shrink: 0;
  `,
  errorIcon: css`
    color: ${theme.colors.error.text};
    font-weight: bold;
    flex-shrink: 0;
  `,
  textStack: css`
    display: flex;
    flex-direction: column;
    overflow: hidden;
  `,
  label: css`
    font-size: ${theme.typography.bodySmall.fontSize};
    font-weight: 500;
    color: ${theme.colors.text.primary};
    white-space: nowrap;
  `,
  status: css`
    font-size: ${theme.typography.bodySmall.fontSize};
    color: ${theme.colors.text.secondary};
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  `,
});
