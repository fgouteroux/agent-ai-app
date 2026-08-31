// Regression tests for MELHORIA-PERFORMANCE-PRODUCAO.md item 2:
// getCurrentDashboard() used to re-fetch the full dashboard JSON from
// Grafana's API on every single call, even though ChatInterface's
// buildAnalysisContext only ever reads its title -- real network cost paid
// on every message sent while looking at the same dashboard. Fixed with a
// uid-keyed cache.

const getMock = jest.fn();
const getVariablesMock = jest.fn(() => []);
const replaceMock = jest.fn((s: string) => s);
const getDataSourceListMock = jest.fn(() => []);

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({ get: getMock }),
  getTemplateSrv: () => ({ getVariables: getVariablesMock, replace: replaceMock }),
  getDataSourceSrv: () => ({ getList: getDataSourceListMock }),
}));

import { contextService } from './context';

function setPath(path: string) {
  window.history.pushState({}, '', path);
}

describe('contextService.getCurrentDashboard', () => {
  beforeEach(() => {
    getMock.mockReset();
    setPath('/');
  });

  it('returns {} without calling the API when there is no dashboard uid in the URL', async () => {
    setPath('/a/shortbobcat2735-agentai-app/chat');
    const result = await contextService.getCurrentDashboard();
    expect(result).toEqual({});
    expect(getMock).not.toHaveBeenCalled();
  });

  it('fetches and returns the dashboard on the first call for a given uid', async () => {
    setPath('/d/dash-first/my-dashboard');
    getMock.mockResolvedValue({ dashboard: { title: 'My Dashboard' } });

    const result = await contextService.getCurrentDashboard();

    expect(result.uid).toBe('dash-first');
    expect(result.title).toBe('My Dashboard');
    expect(getMock).toHaveBeenCalledTimes(1);
  });

  it('does NOT re-fetch on a second call for the same uid', async () => {
    // Distinct uid from the other tests -- dashboardCache is real module
    // state that persists across test cases in this file (not reset by
    // getMock.mockReset()), so reusing a uid another test already fetched
    // would make THIS test's "first" call a cache hit too.
    setPath('/d/dash-repeat/my-dashboard');
    getMock.mockResolvedValue({ dashboard: { title: 'My Dashboard' } });

    await contextService.getCurrentDashboard();
    const second = await contextService.getCurrentDashboard();

    expect(second.title).toBe('My Dashboard');
    expect(getMock).toHaveBeenCalledTimes(1);
  });

  it('fetches again after navigating to a different dashboard uid', async () => {
    setPath('/d/dash-nav-a/my-dashboard');
    getMock.mockResolvedValue({ dashboard: { title: 'My Dashboard' } });
    await contextService.getCurrentDashboard();

    setPath('/d/dash-nav-b/other-dashboard');
    getMock.mockResolvedValue({ dashboard: { title: 'Other Dashboard' } });
    const result = await contextService.getCurrentDashboard();

    expect(result.uid).toBe('dash-nav-b');
    expect(result.title).toBe('Other Dashboard');
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it('does not cache a failed fetch -- retries on the very next call for the same uid', async () => {
    setPath('/d/dash-retry/my-dashboard');
    getMock.mockRejectedValueOnce(new Error('network error'));
    const consoleErrorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});

    const failed = await contextService.getCurrentDashboard();
    expect(failed).toEqual({ uid: 'dash-retry' });

    getMock.mockResolvedValueOnce({ dashboard: { title: 'My Dashboard' } });
    const succeeded = await contextService.getCurrentDashboard();

    expect(succeeded.title).toBe('My Dashboard');
    expect(getMock).toHaveBeenCalledTimes(2);
    consoleErrorSpy.mockRestore();
  });
});
