'use strict'
// Reads {satisfies: [{version, range}], coerce: [{input}]} on stdin and writes
// the answers of the semver module found through NODE_PATH.
const fs = require('fs')
const path = require('path')
const semver = require('semver')

const input = JSON.parse(fs.readFileSync(0, 'utf8'))
const modulePackage = JSON.parse(fs.readFileSync(path.join(process.env.NODE_PATH, 'semver', 'package.json'), 'utf8'))

const out = {
  semverVersion: modulePackage.version,
  satisfies: input.satisfies.map((c) => ({ version: c.version, range: c.range, result: semver.satisfies(c.version, c.range) })),
  coerce: input.coerce.map((c) => {
    const v = semver.coerce(c.input)
    return { input: c.input, result: v === null ? null : v.version }
  }),
}
process.stdout.write(JSON.stringify(out))
