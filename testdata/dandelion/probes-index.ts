import type { Fetcher, FileReader, ProviderUsage } from '../domain/index.ts';
import { agyProbe } from './agy.ts';
import { probeClaudeDeepseek } from './claude-deepseek.ts';
import { probeClaudeOllama } from './claude-ollama.ts';
import { claudeProbe, claudeWorkProbe } from './claude.ts';
import { probeCli, type CliProbe } from './cli.ts';
import { probeCodex, probeCodexAnimus, type CodexIo } from './codex.ts';
import { probeCursor, type CursorIo } from './cursor.ts';
import { probeGrok, type GrokIo } from './grok.ts';
import { probeHermes, type HermesIo } from './hermes.ts';
import { probeJunie, type JunieIo } from './junie.ts';
import { kiloProbe } from './kilo.ts';
import { probeKimi, type KimiIo } from './kimi.ts';
import { probeClaudeStatus, probeCursorStatus, probeOpenAiStatus } from './status.ts';

export type { CommandRunner, CommandRunnerResult, RunFailure } from './cli.ts';
export type { RpcChild, RpcSpawner } from './codex.ts';
export type { LaunchedProcess, Launcher } from './kimi.ts';
export type { Fetcher, FileReader };
export { probeClaudeStatus, probeCursorStatus, probeOpenAiStatus };
export { freshKiloBalance } from './kilo.ts';

export type ProbeIo = KimiIo & GrokIo & JunieIo & CodexIo & CursorIo & HermesIo & { fetcher: Fetcher; reader: FileReader };

export type ProviderProbe = { id: string; probe(now: string): Promise<ProviderUsage> };

function cliProbe(io: ProbeIo, probe: CliProbe): ProviderProbe {
  return { id: probe.id, probe: (now) => probeCli(io, probe, now) };
}

async function guarded(probe: ProviderProbe, now: string): Promise<ProviderUsage> {
  try {
    return await probe.probe(now);
  } catch {
    return { id: probe.id, displayName: probe.id, windows: [], fetchedAt: now, status: 'error', reason: 'probe crashed' };
  }
}

export function providerProbes(io: ProbeIo, env: Record<string, string | undefined>): ProviderProbe[] {
  return rawProbes(io, env).map((probe) => ({ id: probe.id, probe: (now) => guarded(probe, now) }));
}

function rawProbes(io: ProbeIo, env: Record<string, string | undefined>): ProviderProbe[] {
  return [
    cliProbe(io, claudeProbe),
    cliProbe(io, claudeWorkProbe(env, io.reader.homeDir())),
    { id: 'claude-deepseek', probe: (now) => probeClaudeDeepseek(io, env, now) },
    { id: 'claude-ollama', probe: (now) => probeClaudeOllama(io, env, now) },
    cliProbe(io, agyProbe),
    { id: 'kimi', probe: (now) => probeKimi(io, env, now) },
    { id: 'grok', probe: (now) => probeGrok(io, env, now) },
    { id: 'codex', probe: (now) => probeCodex(io, now) },
    { id: 'codex-animus', probe: (now) => probeCodexAnimus(io, env, now) },
    { id: 'cursor', probe: (now) => probeCursor(io, env, now) },
    { id: 'junie', probe: (now) => probeJunie(io, env, now) },
    { id: 'hermes', probe: (now) => probeHermes(io, env, now) },
    cliProbe(io, kiloProbe())
  ];
}
