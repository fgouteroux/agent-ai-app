// External libraries
import { useState, useEffect } from 'react';

import { testConnection } from '../../../../api/client';

/**
 * Return type for the plugin settings hook
 */
interface UsePluginSettingsReturn {
    /** Whether the assistant's own backend is configured (endpoint + model set) */
    llmConfigured: boolean;
    /** Whether the assistant's own backend answered the health check successfully */
    llmHealthy: boolean;
    /** Whether the hook is still loading */
    isLoading: boolean;
    /** Error message if health check failed */
    error: string | null;
}

const RETRY_DELAYS_MS = [1500, 3000, 5000];

/**
 * Checks health of THIS plugin's own backend (agent-ai-app), which owns
 * its LLM endpoint/model configuration directly -- no dependency on the
 * grafana-llm-app plugin or its MCP integration.
 *
 * Retries a few times before giving up: the backend can be mid-restart or its
 * upstream model endpoint still warming up right when this mounts, and a single
 * failed check would otherwise wedge the banner in an error state until the
 * user does a full page reload.
 */
export const usePluginSettings = (): UsePluginSettingsReturn => {
    const [llmConfigured, setLlmConfigured] = useState(false);
    const [llmHealthy, setLlmHealthy] = useState(false);
    const [isLoading, setIsLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        let cancelled = false;

        const attempt = async (retriesLeft: number): Promise<void> => {
            try {
                const health = await testConnection();
                if (cancelled) {
                    return;
                }
                setLlmConfigured(true);
                setLlmHealthy(health.status?.toUpperCase() === 'OK');
                setError(health.status?.toUpperCase() === 'OK' ? null : health.message);
            } catch (e: any) {
                if (cancelled) {
                    return;
                }
                if (retriesLeft > 0) {
                    const delay = RETRY_DELAYS_MS[RETRY_DELAYS_MS.length - retriesLeft] ?? 5000;
                    await new Promise((resolve) => setTimeout(resolve, delay));
                    if (!cancelled) {
                        await attempt(retriesLeft - 1);
                    }
                    return;
                }
                setLlmConfigured(false);
                setLlmHealthy(false);
                setError(e.message || 'Failed to check assistant backend health');
            }
        };

        attempt(RETRY_DELAYS_MS.length).finally(() => {
            if (!cancelled) {
                setIsLoading(false);
            }
        });

        return () => {
            cancelled = true;
        };
    }, []);

    return {
        llmConfigured,
        llmHealthy,
        isLoading,
        error,
    };
};
