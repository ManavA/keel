// Generates reference_grid.json, the answers of the TypeScript reference
// (web/src/demos/permissions/logic.ts in the hanaML repository) for a grid of
// policies and actions, for TestReference_GoldenGrid to hold the Go code to.
//
//   node reference_grid.mts <path to logic.ts> > reference_grid.json
//
// Node 22.18 or later runs a .mts file as it is. The grid is every
// combination, in this order, outermost loop first:
//
//   kind          read, write, send, pay, delete, run
//   kinds[kind]   allow, approve, block (the other kinds keep the defaults below)
//   payLimit      0, 100, 2500
//   external      allow, approve, block
//   sensitive     allow, approve, block
//   amount        absent, 0, 100, 101, 3000
//   action.external   absent, true, false
//   action.sensitive  absent, true, false
//
// The defaults for the kinds that vary no further are read, write, send and pay
// allow; delete and run approve. Each answer is one character of
// "0123456789ab": rule * 3 + decision, where decision is 0 allow, 1 approve,
// 2 block, and rule is 0 for "Default for <kind> actions", 1 for "Payment above
// the $<limit> limit", 2 for "Reaches outside the company" and 3 for "Touches
// sensitive data". That is every rule name the reference can return.
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const { decide } = await import(pathToFileURL(resolve(process.argv[2])).href);

const kinds = ['read', 'write', 'send', 'pay', 'delete', 'run'];
const effects = ['allow', 'approve', 'block'];
const payLimits = [0, 100, 2500];
const amounts = [undefined, 0, 100, 101, 3000];
const flags = [undefined, true, false];
const defaults: Record<string, string> = { read: 'allow', write: 'allow', send: 'allow', pay: 'allow', delete: 'approve', run: 'approve' };
const digits = '0123456789ab';

const ruleCode = (rule: string, kind: string, limit: number): number => {
  if (rule === `Default for ${kind} actions`) return 0;
  if (rule === `Payment above the $${limit.toLocaleString('en-US')} limit`) return 1;
  if (rule === 'Reaches outside the company') return 2;
  if (rule === 'Touches sensitive data') return 3;
  throw new Error(`a rule name the grid does not know: ${rule}`);
};

let answers = '';
for (const kind of kinds)
  for (const kindEffect of effects)
    for (const payLimit of payLimits)
      for (const external of effects)
        for (const sensitive of effects)
          for (const amount of amounts)
            for (const flagExternal of flags)
              for (const flagSensitive of flags) {
                const policy = { kinds: { ...defaults, [kind]: kindEffect }, payLimit, external, sensitive };
                const action: Record<string, unknown> = { id: 'a', agent: 'Agent', kind, summary: 'x' };
                if (amount !== undefined) action.amount = amount;
                if (flagExternal !== undefined) action.external = flagExternal;
                if (flagSensitive !== undefined) action.sensitive = flagSensitive;
                const verdict = decide(action, policy);
                answers += digits[ruleCode(verdict.rule, kind, payLimit) * 3 + effects.indexOf(verdict.decision)];
              }

console.log(JSON.stringify({
  generator: 'reference_grid.mts, run on web/src/demos/permissions/logic.ts; see that file for the order of the grid and the code of each answer',
  cases: answers.length,
  answers,
}, null, 2));
