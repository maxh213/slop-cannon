export type Launch = { command: string; args: string[]; env: Record<string, string> };

export type RunSpawner = { spawn(launch: Launch): Promise<number | 'missing'> };

type Env = Record<string, string | undefined>;

type Target = { command: string; modelFlag: string; effortFlag?: string; effortConfig?: string };

const TARGETS: Record<string, Target> = {
  claude: { command: 'claude', modelFlag: '--model', effortFlag: '--effort' },
  'claude-work': { command: 'claude', modelFlag: '--model', effortFlag: '--effort' },
  'claude-deepseek': { command: 'claude', modelFlag: '--model', effortFlag: '--effort' },
  'claude-ollama': { command: 'claude', modelFlag: '--model', effortFlag: '--effort' },
  agy: { command: 'agy', modelFlag: '--model' },
  kimi: { command: 'kimi', modelFlag: '--model' },
  grok: { command: 'grok', modelFlag: '--model' },
  codex: { command: 'codex', modelFlag: '--model', effortConfig: 'model_reasoning_effort' },
  'codex-animus': { command: 'codex', modelFlag: '--model', effortConfig: 'model_reasoning_effort' },
  cursor: { command: 'cursor-agent', modelFlag: '--model' },
  junie: { command: 'junie', modelFlag: '--model' },
  hermes: { command: 'hermes', modelFlag: '--model' }
};

type AccountHome = { variable: string; name: string; fallback: string };

const ACCOUNT_HOMES: Record<string, AccountHome> = {
  'claude-work': { variable: 'CLAUDE_CONFIG_DIR', name: 'DANDELION_CLAUDE_WORK_CONFIG_DIR', fallback: '.claude-work' },
  'claude-deepseek': { variable: 'CLAUDE_CONFIG_DIR', name: 'DANDELION_CLAUDE_DEEPSEEK_CONFIG_DIR', fallback: '.claude-deepseek' },
  'claude-ollama': { variable: 'CLAUDE_CONFIG_DIR', name: 'DANDELION_CLAUDE_OLLAMA_CONFIG_DIR', fallback: '.claude-ollama' },
  'codex-animus': { variable: 'CODEX_HOME', name: 'DANDELION_CODEX_ANIMUS_HOME', fallback: '.codex-animus' }
};

function accountEnv(account: string, env: Env, homeDir: string): Record<string, string> {
  const home = ACCOUNT_HOMES[account];
  if (home === undefined) return {};
  return { [home.variable]: env[home.name] || `${homeDir}/${home.fallback}` };
}

function effortFlags({ effortFlag, effortConfig }: Target, effort: string | undefined): string[] {
  if (effort === undefined) return [];
  if (effortConfig !== undefined) return ['-c', `${effortConfig}=${effort}`];
  return effortFlag === undefined ? [] : [effortFlag, effort];
}

function flagsOf(target: Target, model: string, effort: string | undefined): string[] {
  return [target.modelFlag, model, ...effortFlags(target, effort)];
}

export function launchOf(routeLine: string, extra: string[], env: Env, homeDir: string): Launch {
  const [model, ...rest] = routeLine.split(' ');
  const account = rest.pop() as string;
  const target = TARGETS[account];
  return { command: target.command, args: [...flagsOf(target, model, rest[0]), ...extra], env: accountEnv(account, env, homeDir) };
}

const POSITIVE_INTEGER = /^[1-9]\d*$/;

export function isMaxAge(value: string | undefined): boolean {
  return POSITIVE_INTEGER.test(String(value));
}

const NUMBER = /^-?\d+(\.\d+)?$/;

function withoutMaxAge(args: string[]): string[] {
  return args.filter((arg, index) => arg !== '--max-age' && !(args[index - 1] === '--max-age' && NUMBER.test(arg)));
}

export function extraArgs(args: string[]): string[] {
  const split = args.indexOf('--');
  const before = withoutMaxAge(split === -1 ? args : args.slice(0, split)).filter((arg) => arg !== '--high');
  return split === -1 ? before : [...before, ...args.slice(split + 1)];
}
