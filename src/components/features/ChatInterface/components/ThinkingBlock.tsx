import React, { useState, useRef, useEffect } from 'react';
import { GrafanaTheme2 } from '@grafana/data';
import { useStyles2, Icon } from '@grafana/ui';
import { css } from '@emotion/css';

interface ThinkingBlockProps {
    content: string;
    isStreaming: boolean;
    thinkingSeconds?: number;
    startTime?: number | null;
}

export const ThinkingBlock: React.FC<ThinkingBlockProps> = ({ isStreaming, thinkingSeconds, startTime }) => {
    const [elapsedSeconds, setElapsedSeconds] = useState(0);
    // Initialize finalSeconds from prop if available (for persisted messages)
    const [finalSeconds, setFinalSeconds] = useState<number | null>(thinkingSeconds ?? null);
    const internalStartTimeRef = useRef<number | null>(null);
    const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
    const styles = useStyles2(getStyles);

    useEffect(() => {
        if (isStreaming) {
            // Use provided startTime or fallback to internal ref
            const effectiveStartTime = startTime || internalStartTimeRef.current || Date.now();

            if (!startTime && internalStartTimeRef.current === null) {
                internalStartTimeRef.current = effectiveStartTime;
            }

            // Clear any existing interval
            if (intervalRef.current) {
                clearInterval(intervalRef.current);
            }

            // Start interval for elapsed time updates
            intervalRef.current = setInterval(() => {
                setElapsedSeconds(Math.floor((Date.now() - effectiveStartTime) / 1000));
            }, 1000);
        } else {
            // Streaming stopped - save final time and reset
            // Using functional setState to capture final time only once
            setFinalSeconds((prev) => {
                if (prev !== null) {
                    return prev;
                }
                const effectiveStartTime = startTime || internalStartTimeRef.current;
                if (effectiveStartTime) {
                    return Math.floor((Date.now() - effectiveStartTime) / 1000);
                }
                return 0;
            });
            internalStartTimeRef.current = null;
            if (intervalRef.current) {
                clearInterval(intervalRef.current);
                intervalRef.current = null;
            }
        }

        // Always return cleanup function
        return () => {
            if (intervalRef.current) {
                clearInterval(intervalRef.current);
            }
        };
    }, [isStreaming, startTime]);

    const displayTime = isStreaming ? elapsedSeconds : (finalSeconds || 0);

    return (
        <div className={styles.thinkingBlockWrapper}>
            <div className={styles.thinkingHeader}>
                <Icon name={isStreaming ? 'spinner' : 'check'} />
                <span className={styles.thinkingLabel}>
                    {isStreaming ? `Analyzing for ${displayTime}s` : `Analysis completed in ${displayTime}s`}
                </span>
            </div>
        </div>
    );
};

const getStyles = (theme: GrafanaTheme2) => ({
    thinkingBlockWrapper: css`
    margin-bottom: ${theme.spacing(1)};
    width: fit-content;
    max-width: 100%;
    border: 1px solid ${theme.colors.border.weak};
    border-radius: 6px;
    background: ${theme.colors.background.primary};
    overflow: hidden;
  `,
    thinkingHeader: css`
    display: flex;
    align-items: center;
    gap: ${theme.spacing(1)};
    padding: ${theme.spacing(1)};
    user-select: none;
    background: ${theme.colors.background.secondary};
  `,
    thinkingLabel: css`
    color: ${theme.colors.text.secondary};
    font-size: ${theme.typography.bodySmall.fontSize};
    white-space: nowrap;
  `,
});
