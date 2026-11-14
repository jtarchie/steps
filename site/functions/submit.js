module.exports = function (request) {
  var result = validate(request.form, {
    email: { type: "email", required: true, maxLen: 200, trim: true },
  });
  if (!result.ok) {
    return response.json({ errors: result.errors }, 400);
  }
  var seq = kv.incr("submission_seq");
  kv.put("submission:" + String(seq).padStart(8, "0"), {
    email: result.data.email,
    ts: Date.now()
  });
  return response.redirect("/thanks.html");
};
