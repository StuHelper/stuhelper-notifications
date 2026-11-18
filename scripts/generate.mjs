import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

function payloadSchemaName(key) {
  assert(key.startsWith('notifications.'), `Title key must start with notifications.: ${key}`);
  return (
    key
      .slice('notifications.'.length)
      .split(/[._]/)
      .map(part => part[0].toUpperCase() + part.slice(1))
      .join('') + 'TitlePayload'
  );
}

function buildParameterProperty(name, contract) {
  if (name === 'reason_code') {
    assert(contract.components.schemas.ReasonCode, 'ReasonCode schema required when reason_code is used');
    return { $ref: '#/components/schemas/ReasonCode' };
  }
  if (name === 'content_name') {
    return {
      type: 'string',
      maxLength: 256,
      readOnly: true,
      description:
        '当前可见的评价标题或资料标题。不可见和依赖故障时为空字符串；客户端按 content_name_state 使用本地化占位，不能回显缓存标题。仅 review、comment、resource 目标提供。',
    };
  }
  if (name === 'content_name_state') {
    return {
      type: 'string',
      enum: ['available', 'inaccessible', 'unavailable'],
      readOnly: true,
      description:
        'available 为当前可读名称；inaccessible 表示已删除、下架或无权查看，统一显示内容不可用；unavailable 为依赖暂不可用。',
    };
  }
  if (name === 'context_id') {
    return {
      type: 'string',
      minLength: 1,
      maxLength: 256,
      readOnly: true,
      description:
        'comment 目标所属根评价 ID，仅在 include_content_names=true 且 target_status=accessible 时返回，用于直达讨论上下文。',
    };
  }
  return {
    type: 'string',
    minLength: 1,
    maxLength: 64,
    pattern: '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$',
    description: '显示用业务编号，不得包含正文、姓名、学号、证件号、邮箱或其他个人敏感信息。',
  };
}

export function synthesizeContract(sourceText) {
  const contract = JSON.parse(sourceText);
  const catalogue = contract['x-title-catalog'];
  assert(catalogue && Object.keys(catalogue).length > 0, 'Missing x-title-catalog');

  const keys = Object.keys(catalogue);
  const usedKinds = new Set();
  for (const rule of Object.values(catalogue)) {
    assert(typeof rule.description === 'string' && rule.description.length > 0, 'Missing rule description');
    assert(['interaction', 'business', 'security', 'account'].includes(rule.type), `Invalid type: ${rule.type}`);
    assert(Array.isArray(rule.target_kinds) && rule.target_kinds.length > 0, 'Missing target_kinds');
    assert(Array.isArray(rule.parameters) && rule.parameters.length > 0, 'Missing parameters');
    for (const kind of rule.target_kinds) {
      usedKinds.add(kind);
    }
  }
  const existingKinds = (contract.components.schemas.Target?.properties?.kind?.enum ?? []).filter(kind => usedKinds.has(kind));
  const targetKinds = [...existingKinds, ...[...usedKinds].filter(kind => !existingKinds.includes(kind))];

  contract.components.schemas.Target.properties.kind.enum = targetKinds;
  contract.components.schemas.TitleKey.enum = keys;
  contract.components.schemas.Notification.allOf[0].oneOf = keys.map(key => ({
    $ref: `#/components/schemas/${payloadSchemaName(key)}`,
  }));

  const coreSchemas = {};
  for (const [name, schema] of Object.entries(contract.components.schemas)) {
    if (!name.endsWith('TitlePayload')) {
      coreSchemas[name] = schema;
    }
  }

  for (const [key, rule] of Object.entries(catalogue)) {
    const paramProperties = {};
    for (const name of [...rule.parameters, ...(rule.optional_parameters ?? []), ...(rule.read_parameters ?? [])]) {
      paramProperties[name] = buildParameterProperty(name, contract);
    }
    coreSchemas[payloadSchemaName(key)] = {
      type: 'object',
      description: rule.description,
      required: ['title_key', 'type', 'target', 'title_params'],
      properties: {
        title_key: {
          type: 'string',
          enum: [key],
        },
        type: {
          type: 'string',
          enum: [rule.type],
        },
        target: {
          type: 'object',
          required: ['kind', 'id'],
          additionalProperties: false,
          properties: {
            kind: {
              type: 'string',
              enum: rule.target_kinds,
            },
            id: {
              type: 'string',
              minLength: 1,
              maxLength: 256,
            },
          },
        },
        title_params: {
          type: 'object',
          required: rule.parameters,
          additionalProperties: false,
          properties: paramProperties,
        },
      },
    };
  }

  contract.components.schemas = coreSchemas;
  return JSON.stringify(contract, null, 2) + '\n';
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const contractPath = process.env.STUHELPER_NOTIFICATIONS_CONTRACT_PATH || 'contracts/notifications.openapi.json';
  const currentContract = fs.readFileSync(contractPath, 'utf8');
  const synthesizedContract = synthesizeContract(currentContract);

  const check = process.argv.includes('--check');
  if (check) {
    assert.equal(
      currentContract,
      synthesizedContract,
      `OpenAPI TitlePayload schemas out of sync with x-title-catalog: run make generate (${contractPath})`
    );
  } else if (currentContract !== synthesizedContract) {
    fs.writeFileSync(contractPath, synthesizedContract);
  }

  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'stuhelper-notifications-generate-'));
  try {
    fs.mkdirSync('sdk', { recursive: true });
    const goOutput = check ? path.join(temporary, 'notifications.gen.go') : 'sdk/notifications.gen.go';
    const tsOutput = check ? path.join(temporary, 'notifications.gen.ts') : 'sdk/notifications.gen.ts';
    execFileSync(
      'go',
      [
        'run',
        'github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0',
        '-generate',
        'types,client',
        '-package',
        'sdk',
        '-o',
        goOutput,
        contractPath,
      ],
      { stdio: 'inherit' }
    );
    execFileSync('pnpm', ['exec', 'openapi-typescript', contractPath, '-o', tsOutput], { stdio: 'inherit' });
    if (check) {
      for (const [expected, actual] of [
        ['sdk/notifications.gen.go', goOutput],
        ['sdk/notifications.gen.ts', tsOutput],
      ]) {
        assert.equal(fs.readFileSync(expected, 'utf8'), fs.readFileSync(actual, 'utf8'), `Generated contract differs: ${expected}`);
      }
    }
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
}
