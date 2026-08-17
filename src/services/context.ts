import { getBackendSrv, getTemplateSrv, getDataSourceSrv } from '@grafana/runtime';

// Import types from centralized location
import type {
    DashboardContext, UserContext, DataSourceContext,
    DashboardSchemaCapability, GrafanaBuildInfo,
} from '../types/context.types';

// Re-export for backward compatibility
export type { DashboardContext, UserContext, DataSourceContext, DashboardSchemaCapability, GrafanaBuildInfo };

// dashboardCache memoizes the last fetched dashboard by uid -- handleSend
// (ChatInterface.tsx) calls getCurrentDashboard() on every single message,
// which used to re-fetch the full dashboard JSON from Grafana's API every
// time even though the caller only ever reads its title. Module-level (not
// per-component state) so it survives across ChatInterface remounts within
// the same browser tab, same lifetime as any other browser-side cache.
let dashboardCache: { uid: string; result: DashboardContext } | null = null;

export const contextService = {
    getDashboardUid(): string | null {
        const path = window.location.pathname;
        // URL format: /d/<uid>/<slug>
        const match = path.match(/\/d\/([^/]+)/);
        return match ? match[1] : null;
    },

    getUserContext(): UserContext {
        const user = (window as any).grafanaBootData?.user ?? {};
        return {
            name: user.name,
            email: user.email,
            login: user.login,
            orgId: user.orgId,
            orgName: user.orgName,
            orgRole: user.orgRole,
        };
    },

    getDataSources(): DataSourceContext[] {
        return getDataSourceSrv().getList().map((ds) => ({
            name: ds.name,
            type: ds.type,
            uid: ds.uid,
        }));
    },

    /**
     * Returns the running Grafana version and derived dashboard schema capability.
     * Reads synchronously from the Grafana boot config — no network call required.
     *
     * Schema capability heuristic:
     *   major ≥ 12 → 'v2-capable' (app-platform / dashboard.grafana.app API may be available)
     *   otherwise  → 'v1' (Classic panels[]/templating.list only)
     *
     * This is a NECESSARY but not SUFFICIENT condition for V2 writes — the MCP server
     * must also support it (mcp-grafana ≥ v0.16.0). The dashboard agent performs an
     * authoritative runtime probe via get_dashboard_by_uid after creating the skeleton.
     */
    getBuildInfo(): GrafanaBuildInfo {
        const version = (window as any).grafanaBootData?.settings?.buildInfo?.version ?? '0.0.0';
        const major = parseInt(version.split('.')[0] ?? '0', 10);
        const dashboardSchema: DashboardSchemaCapability = major >= 12 ? 'v2-capable' : 'v1';
        return { version, dashboardSchema };
    },

    async getCurrentDashboard(): Promise<DashboardContext> {
        const uid = this.getDashboardUid();
        if (!uid) {
            dashboardCache = null;
            return {};
        }

        // Same dashboard as last call (e.g. a second message sent without
        // navigating away) -- skip the network round trip entirely.
        if (dashboardCache && dashboardCache.uid === uid) {
            return dashboardCache.result;
        }

        try {
            const dashboard = await getBackendSrv().get(`/api/dashboards/uid/${uid}`);

            const variables: Record<string, string> = {};
            getTemplateSrv().getVariables().forEach((v: any) => {
                variables[v.name] = getTemplateSrv().replace(`$${v.name}`);
            });

            const result: DashboardContext = {
                uid,
                title: dashboard.dashboard.title,
                json: dashboard.dashboard,
                variables,
            };
            dashboardCache = { uid, result };
            return result;
        } catch (error) {
            console.error('Failed to fetch dashboard context:', error);
            // Deliberately not cached -- a transient fetch failure shouldn't
            // stick around and keep returning just {uid} once the API call
            // would actually succeed again.
            return { uid };
        }
    },
};
