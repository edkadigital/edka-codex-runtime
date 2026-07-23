import { spawn } from 'node:child_process';

const codex = spawn('codex', ['app-server', '--stdio', '--strict-config'], {
    env: process.env,
    stdio: ['pipe', 'pipe', 'pipe'],
});

let stdout = '';
let stderr = '';
let settled = false;

const finish = (error) => {
    if (settled) return;
    settled = true;
    clearTimeout(timeout);
    codex.kill('SIGTERM');
    if (error) {
        console.error(error);
        if (stderr.trim()) console.error(stderr.trim());
        process.exitCode = 1;
    }
};

const timeout = setTimeout(() => finish('timed out waiting for account/read'), 10_000);

codex.stderr.on('data', (chunk) => {
    stderr += chunk;
});

codex.stdout.on('data', (chunk) => {
    stdout += chunk;
    for (;;) {
        const newline = stdout.indexOf('\n');
        if (newline < 0) return;
        const line = stdout.slice(0, newline);
        stdout = stdout.slice(newline + 1);
        if (!line) continue;

        let message;
        try {
            message = JSON.parse(line);
        } catch {
            finish(`app-server returned invalid JSON: ${line}`);
            return;
        }

        if (message.id === 1) {
            codex.stdin.write(`${JSON.stringify({ method: 'initialized', params: {} })}\n`);
            codex.stdin.write(
                `${JSON.stringify({
                    id: 2,
                    method: 'account/read',
                    params: { refreshToken: false },
                })}\n`,
            );
        }

        if (message.id === 2) {
            const account = message.result;
            if (
                account?.account?.type !== 'apiKey' ||
                account?.requiresOpenaiAuth !== true
            ) {
                finish(`unexpected account/read response: ${JSON.stringify(account)}`);
                return;
            }
            console.log('Codex app-server recognizes the non-secret API-key marker');
            finish();
        }
    }
});

codex.on('error', (error) => finish(`failed to start Codex: ${error.message}`));
codex.on('exit', (code, signal) => {
    if (!settled) finish(`Codex exited before account/read (code=${code}, signal=${signal})`);
});

codex.stdin.write(
    `${JSON.stringify({
        id: 1,
        method: 'initialize',
        params: {
            clientInfo: { name: 'edka-runtime-probe', version: '1' },
            capabilities: { experimentalApi: true },
        },
    })}\n`,
);
