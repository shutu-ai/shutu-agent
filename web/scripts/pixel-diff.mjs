import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { inflateSync } from 'node:zlib'

const currentDirectory = resolve(process.env.SHUTU_PIXEL_CURRENT_DIR ?? 'docs/evidence/p36-pixel-current')
const referenceDirectory = resolve(process.env.SHUTU_PIXEL_REFERENCE_DIR ?? 'docs/evidence/p36-pixel-dsh')
const threshold = Number(process.env.SHUTU_PIXEL_DIFF_THRESHOLD ?? 8)
const variants = ['light-desktop', 'dark-desktop', 'light-mobile']

function decodePng(filename) {
  const bytes = readFileSync(filename)
  assert.equal(bytes.readUInt32BE(0), 0x89504e47, `${filename} is not a PNG`)
  let offset = 8
  let width = 0
  let height = 0
  let bitDepth = 0
  let colorType = 0
  const idat = []
  while (offset < bytes.length) {
    const length = bytes.readUInt32BE(offset)
    const type = bytes.toString('ascii', offset + 4, offset + 8)
    const data = bytes.subarray(offset + 8, offset + 8 + length)
    offset += length + 12
    if (type === 'IHDR') {
      width = data.readUInt32BE(0)
      height = data.readUInt32BE(4)
      bitDepth = data[8]
      colorType = data[9]
      assert.equal(data[10], 0, `${filename} uses unsupported compression`)
      assert.equal(data[11], 0, `${filename} uses unsupported filter`)
      assert.equal(data[12], 0, `${filename} uses unsupported interlace`)
    } else if (type === 'IDAT') {
      idat.push(data)
    } else if (type === 'IEND') {
      break
    }
  }
  assert.equal(bitDepth, 8, `${filename} uses unsupported bit depth`)
  assert.ok(colorType === 2 || colorType === 6, `${filename} uses unsupported color type ${colorType}`)
  const bytesPerPixel = colorType === 6 ? 4 : 3
  const stride = width * bytesPerPixel
  const decoded = inflateSync(Buffer.concat(idat))
  const rgba = Buffer.alloc(width * height * 4)
  let sourceOffset = 0
  let previous = Buffer.alloc(stride)
  for (let y = 0; y < height; y += 1) {
    const filter = decoded[sourceOffset++]
    const row = Buffer.from(decoded.subarray(sourceOffset, sourceOffset + stride))
    sourceOffset += stride
    for (let x = 0; x < stride; x += 1) {
      const left = x >= bytesPerPixel ? row[x - bytesPerPixel] : 0
      const up = previous[x]
      const upperLeft = x >= bytesPerPixel ? previous[x - bytesPerPixel] : 0
      if (filter === 1) row[x] = (row[x] + left) & 0xff
      else if (filter === 2) row[x] = (row[x] + up) & 0xff
      else if (filter === 3) row[x] = (row[x] + Math.floor((left + up) / 2)) & 0xff
      else if (filter === 4) {
        const estimate = left + up - upperLeft
        const pa = Math.abs(estimate - left)
        const pb = Math.abs(estimate - up)
        const pc = Math.abs(estimate - upperLeft)
        row[x] = (row[x] + (pa <= pb && pa <= pc ? left : pb <= pc ? up : upperLeft)) & 0xff
      } else assert.equal(filter, 0, `${filename} uses unsupported PNG filter ${filter}`)
    }
    for (let x = 0; x < width; x += 1) {
      const source = x * bytesPerPixel
      const target = (y * width + x) * 4
      rgba[target] = row[source]
      rgba[target + 1] = row[source + 1]
      rgba[target + 2] = row[source + 2]
      rgba[target + 3] = colorType === 6 ? row[source + 3] : 255
    }
    previous = row
  }
  return { width, height, rgba }
}

function compare(current, reference) {
  assert.equal(current.width, reference.width, 'PNG widths differ')
  assert.equal(current.height, reference.height, 'PNG heights differ')
  const pixels = current.width * current.height
  let differingPixels = 0
  let totalAbsoluteDifference = 0
  let maxChannelDifference = 0
  let minX = current.width
  let minY = current.height
  let maxX = -1
  let maxY = -1
  for (let index = 0; index < pixels; index += 1) {
    const offset = index * 4
    let pixelDifference = 0
    for (let channel = 0; channel < 4; channel += 1) {
      const difference = Math.abs(current.rgba[offset + channel] - reference.rgba[offset + channel])
      pixelDifference = Math.max(pixelDifference, difference)
      totalAbsoluteDifference += difference
      maxChannelDifference = Math.max(maxChannelDifference, difference)
    }
    if (pixelDifference > threshold) {
      differingPixels += 1
      const x = index % current.width
      const y = Math.floor(index / current.width)
      minX = Math.min(minX, x)
      minY = Math.min(minY, y)
      maxX = Math.max(maxX, x)
      maxY = Math.max(maxY, y)
    }
  }
  return {
    width: current.width,
    height: current.height,
    threshold,
    differingPixels,
    totalPixels: pixels,
    diffRatio: differingPixels / pixels,
    meanAbsoluteChannelDifference: totalAbsoluteDifference / (pixels * 4),
    maxChannelDifference,
    boundingBox: maxX < 0 ? null : { x: minX, y: minY, width: maxX - minX + 1, height: maxY - minY + 1 },
  }
}

const results = Object.fromEntries(variants.map(variant => [
  variant,
  compare(
    decodePng(resolve(currentDirectory, `reference-${variant}.png`)),
    decodePng(resolve(referenceDirectory, `reference-${variant}.png`)),
  ),
]))
console.log(JSON.stringify({ currentDirectory, referenceDirectory, results }, null, 2))
