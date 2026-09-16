# Runs in the short's work dir (review.yml beside it); build.sh sets PATH, STEPS_DEMO_KEY and the output path.
Require steps
Set Shell bash
Set Width 1080
Set Height 1920
Set FontSize 30
Set Padding 40
Set Theme "Catppuccin Mocha"
Set Framerate 30
Set TypingSpeed 30ms
Set WaitTimeout 120s
Set CursorBlink false
Hide
Type "clear" Enter
Show
Sleep 800ms
Type "steps run review.yml --log-level warn"
Sleep 600ms
Enter
Wait+Screen /usage:/
Sleep 4s
