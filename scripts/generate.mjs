import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

// Keep the public enum, per-key payload schemas, and Sender catalogue identical.
const contract = JSON.parse(fs.readFileSync('contracts/notifications.openapi.json', 'utf8'));
const schemas = contract.components.schemas;
const catalogue = contract['x-title-catalog'];
assert.deepEqual(schemas.TitleKey.enum, Object.keys(catalogue));
const variants = schemas.Notification.allOf[0].oneOf.map(ref => schemas[ref.$ref.split('/').at(-1)]);
assert.equal(variants.length, Object.keys(catalogue).length);
for (const [key, rule] of Object.entries(catalogue)) {
  const variant = variants.find(item => item.properties.title_key.enum[0] === key);
  assert(variant, key);
  assert.deepEqual(variant.properties.type.enum, [rule.type]);
  assert.deepEqual(variant.properties.target.properties.kind.enum, rule.target_kinds);
  assert.deepEqual(variant.properties.title_params.required, rule.parameters);
  assert.deepEqual(Object.keys(variant.properties.title_params.properties), rule.parameters);
  assert.equal(variant.properties.title_params.additionalProperties, false);
}

const check = process.argv.includes('--check');
const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'stuhelper-notifications-generate-'));
try {
  fs.mkdirSync('sdk', { recursive: true });
  const goOutput = check ? path.join(temporary, 'notifications.gen.go') : 'sdk/notifications.gen.go';
  const tsOutput = check ? path.join(temporary, 'notifications.gen.ts') : 'sdk/notifications.gen.ts';
  execFileSync('go', ['run', 'github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0', '-generate', 'types,client', '-package', 'sdk', '-o', goOutput, 'contracts/notifications.openapi.json'], { stdio: 'inherit' });
  execFileSync('pnpm', ['exec', 'openapi-typescript', 'contracts/notifications.openapi.json', '-o', tsOutput], { stdio: 'inherit' });
  if (check) {
    for (const [expected, actual] of [['sdk/notifications.gen.go', goOutput], ['sdk/notifications.gen.ts', tsOutput]]) {
      assert.equal(fs.readFileSync(expected, 'utf8'), fs.readFileSync(actual, 'utf8'), `Generated contract differs: ${expected}`);
    }
  }
} finally {
  fs.rmSync(temporary, { recursive: true, force: true });
}
