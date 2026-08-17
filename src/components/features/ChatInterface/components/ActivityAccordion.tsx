import React, { useState } from 'react';
import { GrafanaTheme2 } from '@grafana/data';
import { useStyles2, Icon } from '@grafana/ui';
import { css } from '@emotion/css';
import { brand } from '../../../../brand';

interface ActivityAccordionProps {
    // Total distinct activity items this turn: thinking (if present) counts
    // as 1, plus one per tool call -- matches how a user would count
    // "things the assistant did" before the real answer.
    stepCount: number;
    // Tool calls that ended in status: 'error'.
    issueCount: number;
    // Still generating (thinking and/or a tool call in flight) this turn.
    isRunning: boolean;
    children: React.ReactNode;
}

// Groups the existing ThinkingBlock + ToolCallContainer under one collapsed-
// by-default summary line, purely to reduce vertical clutter above the real
// answer -- does not change what either child renders or how their own
// individual expand/collapse behaves; this is only ever a wrapper around
// them, rendered unchanged when expanded.
export const ActivityAccordion: React.FC<ActivityAccordionProps> = ({ stepCount, issueCount, isRunning, children }) => {
    const [isExpanded, setIsExpanded] = useState(false);
    const styles = useStyles2(getStyles);

    const summaryText = isRunning
        ? `Working · ${stepCount} step${stepCount === 1 ? '' : 's'}`
        : issueCount > 0
            ? `⚠ Activity · ${issueCount} issue${issueCount === 1 ? '' : 's'}`
            : `✓ Activity · ${stepCount} step${stepCount === 1 ? '' : 's'}`;

    return (
        <div className={styles.wrapper} data-testid="activity-accordion">
            <div
                className={styles.header}
                onClick={() => setIsExpanded(!isExpanded)}
                data-testid="activity-accordion-header"
            >
                {isRunning ? (
                    <Icon name="fa fa-spinner" className={styles.spinner} />
                ) : issueCount > 0 ? (
                    <span className={styles.issueIcon}>⚠</span>
                ) : (
                    <span className={styles.doneIcon}>✓</span>
                )}
                <span className={styles.summaryText}>{summaryText}</span>
                <Icon name={isExpanded ? 'angle-down' : 'angle-right'} size="sm" />
            </div>
            {isExpanded && (
                <div className={styles.content} data-testid="activity-accordion-content">
                    {children}
                </div>
            )}
        </div>
    );
};

const getStyles = (theme: GrafanaTheme2) => ({
    wrapper: css`
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1)};
    margin-bottom: ${theme.spacing(1)};
  `,
    header: css`
    display: inline-flex;
    width: fit-content;
    max-width: 100%;
    align-items: center;
    gap: ${theme.spacing(1)};
    padding: ${theme.spacing(1)};
    border: 1px solid ${theme.colors.border.weak};
    border-radius: 6px;
    background: ${theme.colors.background.secondary};
    font-size: ${theme.typography.bodySmall.fontSize};
    color: ${theme.colors.text.secondary};
    cursor: pointer;
    user-select: none;

    &:hover {
      background: ${theme.colors.action.hover};
    }
  `,
    summaryText: css`
    font-weight: 500;
    white-space: nowrap;
  `,
    spinner: css`
    color: ${brand.red};
    font-size: 14px;
    animation: spin 1s linear infinite;
    @keyframes spin {
      0% { transform: rotate(0deg); }
      100% { transform: rotate(360deg); }
    }
  `,
    doneIcon: css`
    color: ${theme.colors.success.text};
    font-weight: bold;
  `,
    issueIcon: css`
    color: ${theme.colors.warning.text};
    font-weight: bold;
  `,
    content: css`
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1)};
    margin-left: ${theme.spacing(1)};
    padding-left: ${theme.spacing(1)};
    border-left: 2px solid ${theme.colors.border.weak};
  `,
});
