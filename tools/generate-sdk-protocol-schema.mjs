#!/usr/bin/env node

import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDir = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(scriptDir, '..')
if (!process.env.SHUTU_REFERENCE_ROOT) {
  process.env.SHUTU_REFERENCE_ROOT = resolve(repoRoot, '.reference', 'dsh')
}
const referenceRoot = resolve(process.env.SHUTU_REFERENCE_ROOT)
const requireFromReference = createRequire(join(referenceRoot, 'package.json'))
const ts = requireFromReference('typescript')
const referencePackage = JSON.parse(readFileSync(join(referenceRoot, 'package.json'), 'utf8'))

const protocolSource = join(
  referenceRoot,
  'packages',
  'sdk',
  'protocol',
  'src',
  'types.ts',
)

const packageEntries = new Map([
  ['@shutu-ai/dsh-attachment', 'packages/attachment/attachment/src/index.ts'],
  ['@shutu-ai/dsh-brand', 'packages/brand/brand/src/index.ts'],
  ['@shutu-ai/dsh-llm', 'packages/llm/llm/src/index.ts'],
  ['@shutu-ai/dsh-scope', 'packages/scope/scope/src/index.ts'],
  ['@shutu-ai/dsh-session', 'packages/core/session/src/index.ts'],
  ['@shutu-ai/dsh-subagent', 'packages/subagent/subagent/src/index.ts'],
  ['@shutu-ai/dsh-typert-protocol', 'packages/typert/protocol/src/index.ts'],
  ['@shutu-ai/cordis', 'vendor/cordis/src/index.ts'],
])

const basePath = ts.findConfigFile(referenceRoot, ts.sys.fileExists, 'tsconfig.base.json')
if (basePath === undefined) throw new Error('reference tsconfig.base.json not found')
const parsedBase = ts.parseJsonConfigFileContent(
  ts.readJsonConfigFile(basePath, ts.sys.readFile),
  ts.sys,
  dirname(basePath),
)
const compilerOptions = {
  ...parsedBase.options,
  noEmit: true,
  skipLibCheck: true,
  baseUrl: referenceRoot,
  paths: Object.fromEntries([...packageEntries].map(([name, entry]) => [
    name,
    [join(referenceRoot, entry)],
  ])),
}

const program = ts.createProgram([protocolSource], compilerOptions)
const checker = program.getTypeChecker()
const sourceFile = program.getSourceFile(protocolSource)
if (sourceFile === undefined) throw new Error(`cannot load ${protocolSource}`)

const semanticDiagnostics = program.getSemanticDiagnostics(sourceFile)
if (semanticDiagnostics.length !== 0) {
  const rendered = semanticDiagnostics.map(diagnostic => {
    const position = diagnostic.file?.getLineAndCharacterOfPosition(diagnostic.start ?? 0)
    const where = position === undefined ? '' : `${position.line + 1}:${position.character + 1} `
    return where + ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n')
  })
  throw new Error(`reference SDK types have semantic errors:\n${rendered.join('\n')}`)
}

const declarationsByName = new Map()
for (const statement of sourceFile.statements) {
  if (ts.isInterfaceDeclaration(statement)) declarationsByName.set(statement.name.text, statement)
  if (ts.isTypeAliasDeclaration(statement)) declarationsByName.set(statement.name.text, statement)
}

const globalDeclarations = new Map()
for (const file of program.getSourceFiles()) {
  if (!resolve(file.fileName).startsWith(referenceRoot + sep)) continue
  const visit = node => {
    if ((ts.isInterfaceDeclaration(node) || ts.isTypeAliasDeclaration(node)) && node.name.text === node.name.text.trim()) {
      if (!globalDeclarations.has(node.name.text)) globalDeclarations.set(node.name.text, node)
    }
    ts.forEachChild(node, visit)
  }
  visit(file)
}

const definitions = new Map()
const generating = new Set()
const generatedFrom = []
let rootNamedSymbol

function sourceOf(node) {
  const file = node.getSourceFile().fileName
  return relative(referenceRoot, file).replaceAll('\\', '/')
}

function rememberSource(node) {
  const file = sourceOf(node)
  const digest = createHash('sha256').update(readFileSync(resolve(referenceRoot, file))).digest('hex')
  const entry = generatedFrom.find(candidate => candidate.file === file)
  if (entry === undefined) generatedFrom.push({ file, sha256: digest })
}

function declarationName(symbol) {
  const declaration = symbol?.declarations?.find(candidate =>
    ts.isInterfaceDeclaration(candidate) || ts.isTypeAliasDeclaration(candidate))
  return declaration?.name?.text ?? symbol?.name
}

function resolveSymbol(symbol) {
  if (symbol === undefined) return undefined
  return symbol.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(symbol) : symbol
}

function isSchemaableNamedType(symbol) {
  const name = declarationName(symbol)
  return name !== undefined && name !== 'Array' && name !== 'Promise' && name !== 'Record'
}

function referenceForSymbol(symbol) {
  symbol = resolveSymbol(symbol)
  const name = declarationName(symbol)
  if (name === undefined || name === '__type') return undefined
  const declaration = symbol.declarations?.find(candidate =>
    ts.isInterfaceDeclaration(candidate) || ts.isTypeAliasDeclaration(candidate))
  if (declaration === undefined) return undefined
  if (!definitions.has(name)) {
    definitions.set(name, true)
    generating.add(name)
    rememberSource(declaration)
    if (name === 'SessionEvent') {
      definitions.set(name, {
        type: 'object',
        required: ['type', 'seq', 'time'],
        properties: {
          type: { type: 'string' },
          seq: { type: 'integer', minimum: 0 },
          time: { type: 'integer', minimum: 0 },
          data: true,
          ignorable: { const: true },
          sourceEventSeqs: { type: 'array', items: { type: 'integer', minimum: 0 } },
          surfaceOp: {
            oneOf: [
              { const: 'append' },
              {
                type: 'object',
                required: ['op', 'start', 'end'],
                properties: {
                  op: { const: 'replace' },
                  start: { type: 'number' },
                  end: { type: 'number' },
                },
              },
            ],
          },
        },
      })
      generating.delete(name)
      return { $ref: `#/$defs/${name}` }
    }
    if (name === 'AttachmentId' || name === 'CallId') {
      definitions.set(name, { type: 'string' })
      generating.delete(name)
      return { $ref: `#/$defs/${name}` }
    }
    if (name === 'ContentBlock') {
      const blockNames = ['TextBlock', 'ReasoningBlock', 'ImageBlock', 'ToolCallBlock', 'ToolResultBlock']
      const blockTypes = ['text', 'reasoning', 'image', 'tool-call', 'tool-result']
      for (const blockName of blockNames) defineGlobalDeclaration(blockName)
      const knownBlocks = blockNames.map(blockName => ({ $ref: `#/$defs/${blockName}` }))
      definitions.set(name, {
        oneOf: [...knownBlocks, {
          type: 'object',
          required: ['type'],
          properties: { type: { type: 'string' } },
          not: {
            required: ['type'],
            properties: { type: { enum: blockTypes } },
          },
        }],
      })
      generating.delete(name)
      return { $ref: `#/$defs/${name}` }
    }
    definitions.set(name, declarationSchema(declaration))
    generating.delete(name)
  }
  return { $ref: `#/$defs/${name}` }
}

function defineGlobalDeclaration(name) {
  if (definitions.has(name)) return
  const declaration = globalDeclarations.get(name)
  if (declaration === undefined) {
    throw new Error(`cannot find reference declaration ${name}; known=${[...globalDeclarations.keys()].join(',')}`)
  }
  referenceForDeclaration(name, declaration)
}

function referenceForDeclaration(name, declaration) {
  definitions.set(name, true)
  generating.add(name)
  rememberSource(declaration)
  definitions.set(name, declarationSchema(declaration))
  generating.delete(name)
}

function schemaFromNode(node) {
  if (node === undefined) return true
  if (ts.isTypeReferenceNode(node)) {
    const symbol = resolveSymbol(checker.getSymbolAtLocation(node.typeName))
    if (symbol !== undefined && isSchemaableNamedType(symbol)) {
      return referenceForSymbol(symbol)
    }
  }
  if (ts.isArrayTypeNode(node)) {
    return { type: 'array', items: schemaFromNode(node.elementType) }
  }
  if (ts.isTupleTypeNode(node)) {
    return { type: 'array', items: oneOfOrSchema(node.elements.map(schemaFromNode)), minItems: node.elements.length }
  }
  if (ts.isUnionTypeNode(node)) return oneOfOrSchema(node.types.map(schemaFromNode))
  if (ts.isIntersectionTypeNode(node)) {
    return node.types.map(schemaFromNode).reduce(mergeObjectSchemas, {})
  }
  if (ts.isParenthesizedTypeNode(node)) return schemaFromNode(node.type)
  if (ts.isTypeOperatorNode(node)) return schemaFromNode(node.type)
  if (ts.isLiteralTypeNode(node)) {
    if (node.literal.kind === ts.SyntaxKind.StringLiteral) return { const: node.literal.text }
    if (node.literal.kind === ts.SyntaxKind.NumericLiteral) return { const: Number(node.literal.text) }
    if (node.literal.kind === ts.SyntaxKind.TrueKeyword) return { const: true }
    if (node.literal.kind === ts.SyntaxKind.FalseKeyword) return { const: false }
  }
  if (node.kind === ts.SyntaxKind.StringKeyword) return { type: 'string' }
  if (node.kind === ts.SyntaxKind.NumberKeyword) return { type: 'number' }
  if (node.kind === ts.SyntaxKind.BooleanKeyword) return { type: 'boolean' }
  if (node.kind === ts.SyntaxKind.UndefinedKeyword || node.kind === ts.SyntaxKind.NeverKeyword) return false
  if (ts.isTypeLiteralNode(node)) return schemaFromType(checker.getTypeAtLocation(node))
  return schemaFromType(checker.getTypeAtLocation(node))
}

function schemaFromType(type) {
  if (type.flags & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) return true
  if (type.flags & ts.TypeFlags.String) return { type: 'string' }
  if (type.flags & ts.TypeFlags.Number) return { type: 'number' }
  if (type.flags & ts.TypeFlags.Boolean) return { type: 'boolean' }
  if (type.flags & ts.TypeFlags.StringLiteral && typeof type.value === 'string') return { const: type.value }
  if (type.flags & ts.TypeFlags.NumberLiteral && typeof type.value === 'number') return { const: type.value }
  if (type.flags & ts.TypeFlags.Union) return oneOfOrSchema(type.types.map(schemaFromType))

  const namedReference = isSchemaableNamedType(type.symbol) && type.symbol !== rootNamedSymbol
    ? referenceForSymbol(type.symbol)
    : undefined
  if (namedReference !== undefined) return namedReference

  if (type.flags & ts.TypeFlags.Intersection) {
    return type.types.map(schemaFromType).reduce(mergeObjectSchemas, {})
  }
  if (checker.isArrayType(type)) {
    return { type: 'array', items: schemaFromType(checker.getTypeArguments(type)[0] ?? type.getStringType()) }
  }
  if (checker.isTupleType(type)) {
    const elements = checker.getTypeArguments(type)
    return { type: 'array', items: oneOfOrSchema(elements.map(schemaFromType)), minItems: elements.length }
  }

  const callSignatures = type.getCallSignatures()
  if (callSignatures.length !== 0) throw new Error('callable SDK protocol types cannot be represented as JSON')
  const properties = {}
  const required = []
  for (const property of type.getProperties()) {
    const propertyNode = property.valueDeclaration
    if (propertyNode === undefined || property.name === '__type') continue
    const propertySchema = schemaFromType(checker.getTypeOfSymbolAtLocation(property, propertyNode))
    if (propertySchema === false) continue
    properties[property.name] = propertySchema
    if (propertyNode.questionToken === undefined) required.push(property.name)
  }
  const objectSchema = { type: 'object', properties }
  if (required.length !== 0) objectSchema.required = required
  const indexInfo = type.getNumberIndexType()
  if (indexInfo !== undefined) objectSchema.items = schemaFromType(indexInfo)
  return objectSchema
}

function oneOfOrSchema(schemas) {
  const filtered = schemas.filter(schema => schema !== false)
  if (filtered.length === 0) return false
  if (filtered.length === 1) return filtered[0]
  return { oneOf: filtered }
}

function mergeObjectSchemas(left, right) {
  if (left === true || right === true) return true
  if (!('type' in left) || !('type' in right)) return oneOfOrSchema([left, right])
  const properties = { ...(left.properties ?? {}), ...(right.properties ?? {}) }
  const required = [...new Set([...(left.required ?? []), ...(right.required ?? [])])]
  const result = { type: 'object', properties }
  if (required.length !== 0) result.required = required
  return result
}

function definition(name) {
  const declaration = declarationsByName.get(name)
  if (declaration === undefined) throw new Error(`missing SDK protocol declaration ${name}`)
  rememberSource(declaration)
  generating.add(name)
  const schema = declarationSchema(declaration)
  generating.delete(name)
  definitions.set(name, schema)
  return { $ref: `#/$defs/${name}` }
}

function declarationSchema(declaration) {
  const previousRoot = rootNamedSymbol
  rootNamedSymbol = checker.getSymbolAtLocation(declaration.name)
  try {
    return ts.isInterfaceDeclaration(declaration)
      ? schemaFromInterfaceDeclaration(declaration)
      : schemaFromNode(declaration.type)
  } finally {
    rootNamedSymbol = previousRoot
  }
}

function schemaFromInterfaceDeclaration(declaration) {
  const properties = {}
  const required = []
  for (const member of declaration.members) {
    if (ts.isPropertySignature(member) && member.type !== undefined) {
      const schema = schemaFromNode(member.type)
      if (schema === false) continue
      properties[member.name.text] = schema
      if (member.questionToken === undefined) required.push(member.name.text)
    }
  }
  const schema = { type: 'object', properties }
  if (required.length !== 0) schema.required = required
  return schema
}

for (const name of [
  'InitializeParams',
  'InitializeResult',
  'SessionPromptParams',
  'SessionPromptResult',
  'SdkRunStatus',
  'SessionEventNotification',
  'SessionStatusNotification',
  'SubagentStartedNotification',
  'SubagentFinishedNotification',
]) definition(name)

// The reference TS number/string types are deliberately wider than invariants
// enforced at the SDK server boundary.
definitions.get('InitializeParams').properties.maxTokens = {
  type: 'integer',
  minimum: 1,
}
definitions.get('SessionPromptResult').properties.messageId.minLength = 1

function mapMembers(name) {
  const declaration = declarationsByName.get(name)
  if (declaration === undefined || !ts.isInterfaceDeclaration(declaration)) {
    throw new Error(`${name} must be an interface map`)
  }
  return declaration.members.filter(ts.isPropertySignature)
}

function memberName(member) {
  const name = ts.getNameOfDeclaration(member)
  if (name === undefined || !ts.isStringLiteral(name)) throw new Error('SDK map key must be a string literal')
  return name.text
}

function definitionPointer(name) {
  return `#/$defs/${name.replaceAll('~', '~0').replaceAll('/', '~1')}`
}

function referencedDeclarationName(node) {
  if (!ts.isTypeReferenceNode(node)) return undefined
  return resolveSymbol(checker.getSymbolAtLocation(node.typeName))?.name
}

const notificationFrames = []
for (const member of mapMembers('HarnessSdkNotificationMap')) {
  const method = memberName(member)
  const paramsName = referencedDeclarationName(member.type)
  if (paramsName === undefined) throw new Error(`notification ${method} has no named params type`)
  const frameName = `notification.${method}`
  definitions.set(frameName, {
    type: 'object',
    required: ['jsonrpc', 'method', 'params'],
    properties: {
      jsonrpc: { const: '2.0' },
      method: { const: method },
      params: { $ref: `#/$defs/${paramsName}` },
    },
    additionalProperties: false,
  })
  notificationFrames.push({ $ref: definitionPointer(frameName) })
}

const requestFrames = []
for (const member of mapMembers('HarnessSdkRequestMap')) {
  const method = memberName(member)
  if (!ts.isTypeLiteralNode(member.type)) throw new Error(`request ${method} shape must be inline`)
  const paramsMember = member.type.members.find(candidate =>
    ts.isPropertySignature(candidate) && candidate.name.getText() === 'params')
  const resultMember = member.type.members.find(candidate =>
    ts.isPropertySignature(candidate) && candidate.name.getText() === 'result')
  if (paramsMember?.type === undefined || resultMember?.type === undefined) {
    throw new Error(`request ${method} must declare params and result`)
  }
  const paramsName = referencedDeclarationName(paramsMember.type)
  const resultName = referencedDeclarationName(resultMember.type)
  if (resultName === undefined) throw new Error(`request ${method} result has no named type`)
  const properties = {
    jsonrpc: { const: '2.0' },
    id: { type: 'string', minLength: 1 },
    method: { const: method },
  }
  const required = ['jsonrpc', 'id', 'method']
  if (paramsName !== undefined) {
    properties.params = { $ref: `#/$defs/${paramsName}` }
    required.push('params')
  }
  const frameName = `request.${method}`
  definitions.set(frameName, {
    type: 'object',
    required,
    properties,
    additionalProperties: false,
  })
  requestFrames.push({ $ref: definitionPointer(frameName) })
}

// Shutu's optional session projection query is deliberately kept outside the
// reference request map: reference clients can ignore it, while shutu's own
// SDK can request the same durable Snapshot consumed by Native/Web. Keep its
// request shape in the generated schema so local contract validation does not
// reject an implemented extension.
definitions.set('SessionSnapshotParams', {
  type: 'object',
  required: ['sessionId'],
  properties: { sessionId: { type: 'string', minLength: 1 } },
  additionalProperties: false,
})
definitions.set('request.session/snapshot', {
  type: 'object',
  required: ['jsonrpc', 'id', 'method', 'params'],
  properties: {
    jsonrpc: { const: '2.0' },
    id: { type: 'string', minLength: 1 },
    method: { const: 'session/snapshot' },
    params: { $ref: '#/$defs/SessionSnapshotParams' },
  },
  additionalProperties: false,
})
requestFrames.push({ $ref: definitionPointer('request.session/snapshot') })

definitions.set('notification', { oneOf: notificationFrames })
definitions.set('request', { oneOf: requestFrames })
definitions.set('initializeResult', { $ref: '#/$defs/InitializeResult' })
definitions.set('promptResult', { $ref: '#/$defs/SessionPromptResult' })
definitions.set('shutdownResult', { type: 'object', properties: {}, additionalProperties: false })

const schema = {
  $schema: 'https://json-schema.org/draft/2020-12/schema',
  $defs: Object.fromEntries([...definitions].filter(([name]) => name !== 'Record')),
  'x-generated-by': 'tools/generate-sdk-protocol-schema.mjs',
  'x-reference': `${referencePackage.name}@${referencePackage.version}`,
  'x-generated-from': generatedFrom.sort((left, right) => left.file.localeCompare(right.file)),
}

const outputPath = process.argv[2] ?? join(
  repoRoot,
  'internal',
  'sdkclient',
  'testdata',
  'protocol.schema.json',
)
writeFileSync(outputPath, `${JSON.stringify(schema, null, 2)}\n`)
console.log(`generated ${relative(repoRoot, outputPath)} from ${relative(repoRoot, protocolSource)}`)
