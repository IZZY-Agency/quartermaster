// qm-privcheck: the on-device half of quartermaster's privacy gate (macOS).
//
//   qm-privcheck names   stdin [{"id","text"}]                 -> stdout [{"id","names":[...]}]
//   qm-privcheck ask     stdin [{"key","instructions","prompt"}] -> stdout [{"key","answer"}]
//   qm-privcheck status  prints "available" (exit 0) or "unavailable: <reason>" (exit 3)
//
// names uses NaturalLanguage's name tagger (personal names only). ask and
// status use the Apple Foundation Model; when it is not available they exit 3.
// Any other failure exits 1 with a message on stderr.
import Foundation
import NaturalLanguage
#if canImport(FoundationModels)
import FoundationModels
#endif

func fail(_ message: String, _ code: Int32 = 1) -> Never {
  FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
  exit(code)
}

func readItems() -> [[String: String]] {
  let data = FileHandle.standardInput.readDataToEndOfFile()
  guard let items = try? JSONSerialization.jsonObject(with: data) as? [[String: String]] else {
    fail("stdin is not a JSON array of string objects")
  }
  return items
}

func write(_ value: Any) {
  guard let data = try? JSONSerialization.data(withJSONObject: value) else { fail("cannot encode output") }
  FileHandle.standardOutput.write(data)
  FileHandle.standardOutput.write("\n".data(using: .utf8)!)
}

func names() {
  var out: [[String: Any]] = []
  for item in readItems() {
    guard let id = item["id"], let text = item["text"] else { fail("names: each item needs id and text") }
    let tagger = NLTagger(tagSchemes: [.nameType])
    tagger.string = text
    var found: [String] = []
    tagger.enumerateTags(in: text.startIndex..<text.endIndex, unit: .word, scheme: .nameType,
                         options: [.omitPunctuation, .omitWhitespace, .joinNames]) { tag, range in
      if tag == .personalName { found.append(String(text[range])) }
      return true
    }
    out.append(["id": id, "names": found])
  }
  write(out)
}

/// The reason the model is unavailable, or nil when it is available.
func unavailableReason() -> String? {
  #if canImport(FoundationModels)
  if #available(macOS 26.0, *) {
    switch SystemLanguageModel.default.availability {
    case .available: return nil
    case .unavailable(let reason): return String(describing: reason)
    @unknown default: return "unknown"
    }
  }
  return "needs macOS 26 or later"
  #else
  return "built without FoundationModels"
  #endif
}

func ask() async {
  if unavailableReason() != nil { fail("unavailable", 3) }
  let items = readItems()
  #if canImport(FoundationModels)
  if #available(macOS 26.0, *) {
    var out: [[String: String]] = []
    let options = GenerationOptions(samplingMode: .greedy, maximumResponseTokens: 4)
    for item in items {
      guard let key = item["key"], let instructions = item["instructions"], let prompt = item["prompt"] else {
        fail("ask: each item needs key, instructions and prompt")
      }
      let session = LanguageModelSession(instructions: instructions)
      do {
        let reply = try await session.respond(to: prompt, options: options)
        out.append(["key": key, "answer": reply.content])
      } catch {
        fail("ask: \(key): \(error)")
      }
    }
    write(out)
    return
  }
  #endif
  fail("unavailable", 3)
}

let args = CommandLine.arguments
switch args.count == 2 ? args[1] : "" {
case "names":
  names()
case "ask":
  await ask()
case "status":
  if let reason = unavailableReason() {
    print("unavailable: \(reason)")
    exit(3)
  }
  print("available")
default:
  fail("usage: qm-privcheck names|ask|status", 2)
}
